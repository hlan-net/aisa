package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/models"
)

// ModelGVR is the resource of the Model objects (KUBERNETES_OBJECTS.md, Model). The CRD is in
// aisa's chart.
var ModelGVR = schema.GroupVersionResource{Group: "aisa.hlan.net", Version: "v1alpha1", Resource: "models"}

const modelKind = "Model"

// Prefixes of the labels that a selector can match a Model's features and capabilities by.
const (
	labelFeature    = "feature.aisa.hlan.net/"
	labelCapability = "capability.aisa.hlan.net/"
)

// timeoutMeta is what the proxy's template takes as a backend's timeout; a backend with another
// value is left out of the gateway's config, and so out of the models on offer.
var timeoutMeta = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// pathMeta is what the proxy's template takes as the base path of a backend's API (#91): one or
// more segments, each after a '/', none of them "." or "..".
var pathMeta = regexp.MustCompile(`^(/[A-Za-z0-9._~@:+-]+)+$`)

// keyPathMeta and keyFieldMeta are what the template takes as the Vault path and the field of a
// credential that Vault issues (#92); key_path also needs key, the account's name.
var (
	keyPathMeta  = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	keyFieldMeta = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// HealthReader reads the backends registered in the catalog; consul.Client implements it.
type HealthReader interface {
	Health(ctx context.Context, service string) ([]consul.Instance, error)
}

// ModelsOptions configure Models.
type ModelsOptions struct {
	// Service is the backends' service name in the catalog, "aisa-backend".
	Service string
	// Descriptions lists the models' descriptions, aisa/models/<model> in Consul KV.
	Descriptions consul.Lister
	// Endpoint is the proxy's URL that applications call, spec.endpoint.
	Endpoint string
	// Install names this aisa install on the objects it writes.
	Install string
	// EmptyGrace is how long the catalog may offer no model before the objects are deleted:
	// it can come back empty although the backends are fine, and the proxy keeps its model
	// routes for as long (PROXY_CONTRACT.md, contract 3).
	EmptyGrace time.Duration
	// Now is the clock; time.Now by default.
	Now func() time.Time
	// OnPass is called after every pass that read Consul and changed the objects to match.
	OnPass func(ModelStats)
}

// ModelStats describe a pass, for metrics.
type ModelStats struct {
	// Models is how many Model objects this install keeps, Available how many of them have a
	// healthy backend.
	Models, Available int
	// Conflicts counts the models left out because their derived names collide, or because an
	// object of that name is not this install's.
	Conflicts int
	// InvalidDescriptions counts the descriptions that could not be read; their models get the
	// default description.
	InvalidDescriptions int
}

// Models keeps one Model object per model that the proxy offers.
type Models struct {
	catalog HealthReader
	dyn     dynamic.Interface
	opts    ModelsOptions
	log     *slog.Logger
	// emptySince is when the catalog began to offer no model; zero while it offers one.
	emptySince time.Time
}

// NewModels returns Models; call Reconcile for each pass.
func NewModels(catalog HealthReader, dyn dynamic.Interface, log *slog.Logger, opts ModelsOptions) *Models {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Models{catalog: catalog, dyn: dyn, opts: opts, log: log}
}

// offered is a model on offer, as its object is to say.
type offered struct {
	name      string
	available bool
	desc      models.Description
}

// Reconcile makes one pass: it reads the catalog and the descriptions, then creates, updates and
// deletes this install's Model objects to match. When Consul cannot be read nothing changes, and
// a catalog that offers no model changes nothing until it has done so for EmptyGrace.
func (m *Models) Reconcile(ctx context.Context) error {
	instances, err := m.catalog.Health(ctx, m.opts.Service)
	if err != nil {
		return fmt.Errorf("read the backends: %w", err)
	}
	pairs, _, err := m.opts.Descriptions.List(ctx, 0)
	if err != nil {
		return fmt.Errorf("read the model descriptions: %w", err)
	}
	stats := ModelStats{}
	ix := m.descriptions(pairs, instances, &stats)
	offer := offeredModels(instances, ix)
	if m.holdEmpty(len(offer)) {
		return nil
	}
	keep, errs := m.applyAll(ctx, offer, &stats)

	if err := m.removeStale(ctx, keep); err != nil {
		errs = append(errs, err)
	}
	if m.opts.OnPass != nil {
		m.opts.OnPass(stats)
	}
	return errors.Join(errs...)
}

// holdEmpty reports whether a catalog of no models is to be waited out: the first time, and
// until EmptyGrace has passed since.
func (m *Models) holdEmpty(offered int) bool {
	if offered > 0 {
		m.emptySince = time.Time{}
		return false
	}
	now := m.opts.Now()
	if m.emptySince.IsZero() {
		m.emptySince = now
	}
	if wait := m.emptySince.Add(m.opts.EmptyGrace).Sub(now); wait > 0 {
		m.log.Warn("the catalog offers no model; the Model objects stay until the grace period ends",
			"remaining", wait.Round(time.Second).String())
		return true
	}
	return false
}

// descriptions reads the models' descriptions against the catalog; one that cannot be read, or
// whose aliases collide (#89), is logged and counted, and its model gets the default one.
func (m *Models) descriptions(pairs []consul.Pair, instances []consul.Instance, stats *ModelStats) models.Index {
	ix := models.NewIndex(pairs, models.Names(instances))
	for _, name := range ix.InvalidNames {
		m.log.Error("invalid model description; the model gets the default one", "model", name, "error", ix.Invalid[name])
	}
	stats.InvalidDescriptions = len(ix.Invalid)
	return ix
}

// offeredModels returns the models of the backends the proxy renders, each available when one
// of its backends passes its checks.
func offeredModels(instances []consul.Instance, ix models.Index) map[string]*offered {
	offer := map[string]*offered{}
	for _, in := range instances {
		if !usable(in.Meta) {
			continue
		}
		for name := range strings.SplitSeq(in.Meta["models"], ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			o, ok := offer[name]
			if !ok {
				o = &offered{name: name, desc: ix.Of(name)}
				offer[name] = o
			}
			o.available = o.available || in.Passing
		}
	}
	return offer
}

// applyAll creates or updates the object of each model, except for names that derive to one
// object name, where neither gets one. It returns the objects to keep.
func (m *Models) applyAll(ctx context.Context, models map[string]*offered, stats *ModelStats) (map[string]bool, []error) {
	byObject := map[string][]string{}
	for name := range models {
		obj := DerivedName(name, "model")
		byObject[obj] = append(byObject[obj], name)
	}
	var errs []error
	keep := map[string]bool{}
	for _, obj := range slices.Sorted(maps.Keys(byObject)) {
		names := byObject[obj]
		if len(names) > 1 {
			sort.Strings(names)
			m.log.Error("model names derive to one object name; none of them gets an object", "models", names, "object", obj)
			stats.Conflicts += len(names)
			continue
		}
		o := models[names[0]]
		owned, err := m.apply(ctx, obj, o)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("model %q: %w", o.name, err))
			keep[obj] = true
		case !owned:
			// Left alone: not changed here, and not deleted as stale either.
			m.log.Error("a Model object of that name exists and is not this install's; it is left alone", "model", o.name, "object", obj)
			stats.Conflicts++
			keep[obj] = true
		default:
			keep[obj] = true
			stats.Models++
			if o.available {
				stats.Available++
			}
		}
	}
	return keep, errs
}

// usable reports whether the proxy's template renders a backend (proxies/apisix/apisix.yaml.ctmpl):
// one with models, the provider openai-compatible, and a valid timeout, path and key_path if any.
// Whether the credential at key_path has its field is the template's to see: aisa does not read it.
func usable(meta map[string]string) bool {
	if strings.TrimSpace(meta["models"]) == "" || meta["provider"] != "openai-compatible" {
		return false
	}
	if t := meta["timeout"]; t != "" && !timeoutMeta.MatchString(t) {
		return false
	}
	if kp := meta["key_path"]; kp != "" && (meta["key"] == "" || !keyFieldMeta.MatchString(meta["key_field"]) ||
		!keyPathMeta.MatchString(kp) || hasDotSegment(kp)) {
		return false
	}
	return validPath(meta["path"])
}

// hasDotSegment reports whether a path has a segment "." or "..".
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// validPath reports whether a backend's meta path is absent or one the template takes.
func validPath(p string) bool {
	if p == "" {
		return true
	}
	return pathMeta.MatchString(p) && !hasDotSegment(p)
}

// desired returns the object's spec and the labels aisa sets on it.
func (m *Models) desired(obj string, o *offered) (map[string]any, map[string]string) {
	spec := map[string]any{
		"model":    o.name,
		"features": toAny(o.desc.Features),
		"endpoint": m.opts.Endpoint,
	}
	if o.desc.ContextLength > 0 {
		spec["contextLength"] = o.desc.ContextLength
	}
	if len(o.desc.Capabilities) > 0 {
		spec["capabilities"] = toAny(o.desc.Capabilities)
	}
	l := map[string]string{
		LabelManagedBy: ManagedBy,
		LabelInstall:   m.opts.Install,
		LabelModel:     obj,
	}
	for _, f := range o.desc.Features {
		l[labelFeature+f] = "true"
	}
	for _, c := range o.desc.Capabilities {
		l[labelCapability+c] = "true"
	}
	return spec, l
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// apply creates or updates the object of a model. It returns false, and changes nothing, when
// an object of that name exists and is not this install's.
func (m *Models) apply(ctx context.Context, obj string, o *offered) (bool, error) {
	spec, wantLabels := m.desired(obj, o)
	res := m.dyn.Resource(ModelGVR)
	cur, err := res.Get(ctx, obj, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": ModelGVR.GroupVersion().String(),
			"kind":       modelKind,
			"metadata": map[string]any{
				"name":        obj,
				"labels":      toAnyMap(wantLabels),
				"annotations": map[string]any{AnnotationModel: o.name},
			},
			"spec": spec,
		}}
		cur, err = res.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return true, fmt.Errorf("create: %w", err)
		}
		return true, m.setStatus(ctx, cur, o.available)
	}
	if err != nil {
		return true, fmt.Errorf("get: %w", err)
	}
	l := cur.GetLabels()
	if l[LabelManagedBy] != ManagedBy || l[LabelInstall] != m.opts.Install || cur.GetAnnotations()[AnnotationModel] != o.name {
		return false, nil
	}

	// Labels aisa does not set are kept; its own are made exactly the wanted ones.
	newLabels := map[string]string{}
	for k, v := range l {
		if !strings.HasPrefix(k, labelFeature) && !strings.HasPrefix(k, labelCapability) {
			newLabels[k] = v
		}
	}
	maps.Copy(newLabels, wantLabels)
	curSpec, _, _ := unstructured.NestedMap(cur.Object, "spec")
	if !reflect.DeepEqual(normalize(curSpec), normalize(spec)) || !maps.Equal(l, newLabels) {
		next := cur.DeepCopy()
		next.SetLabels(newLabels)
		next.Object["spec"] = spec
		cur, err = res.Update(ctx, next, metav1.UpdateOptions{})
		if err != nil {
			return true, fmt.Errorf("update: %w", err)
		}
	}
	return true, m.setStatus(ctx, cur, o.available)
}

// setStatus writes status.available when it differs.
func (m *Models) setStatus(ctx context.Context, cur *unstructured.Unstructured, available bool) error {
	if got, found, _ := unstructured.NestedBool(cur.Object, "status", "available"); found && got == available {
		return nil
	}
	next := cur.DeepCopy()
	if err := unstructured.SetNestedField(next.Object, available, "status", "available"); err != nil {
		return err
	}
	if _, err := m.dyn.Resource(ModelGVR).UpdateStatus(ctx, next, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

// removeStale deletes this install's objects of models no longer on offer.
func (m *Models) removeStale(ctx context.Context, keep map[string]bool) error {
	selector := labels.Set{LabelManagedBy: ManagedBy, LabelInstall: m.opts.Install}.String()
	list, err := m.dyn.Resource(ModelGVR).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list the Model objects: %w", err)
	}
	var errs []error
	for _, item := range list.Items {
		if keep[item.GetName()] {
			continue
		}
		uid := item.GetUID()
		err := m.dyn.Resource(ModelGVR).Delete(ctx, item.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete the Model object %s: %w", item.GetName(), err))
			continue
		}
		m.log.Info("deleted the Model object of a model no longer on offer", "object", item.GetName(),
			"model", item.GetAnnotations()[AnnotationModel])
	}
	return errors.Join(errs...)
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalize makes a spec comparable whether it came from the API server (numbers as int64 or
// float64) or was built here. Numbers stay exact (json.Number): two integers above 2^53 that
// differ compare as different.
func normalize(v map[string]any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	if dec.Decode(&out) != nil {
		return v
	}
	return out
}
