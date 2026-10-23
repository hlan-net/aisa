package kube

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/hlan-net/aisa/internal/consul"
)

type fakeCatalog struct {
	instances []consul.Instance
	err       error
}

func (c *fakeCatalog) Health(context.Context, string) ([]consul.Instance, error) {
	return c.instances, c.err
}

type fakeKV struct {
	pairs []consul.Pair
	err   error
}

func (f *fakeKV) List(context.Context, uint64) ([]consul.Pair, uint64, error) {
	return f.pairs, 1, f.err
}

type modelsTest struct {
	t       *testing.T
	catalog *fakeCatalog
	descs   *fakeKV
	dyn     *dynamicfake.FakeDynamicClient
	stats   ModelStats
	models  *Models
}

const endpoint = "http://aisa-apisix.aisa.svc.cluster.local/v1"

func newModelsTest(t *testing.T, instances ...consul.Instance) *modelsTest {
	mt := &modelsTest{
		t:       t,
		catalog: &fakeCatalog{instances: instances},
		descs:   &fakeKV{},
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{ModelGVR: "ModelList"}),
	}
	mt.models = NewModels(mt.catalog, mt.dyn, slog.New(slog.DiscardHandler), ModelsOptions{
		Service: "aisa-backend", Descriptions: mt.descs, Endpoint: endpoint, Install: "aisa",
		OnPass: func(s ModelStats) { mt.stats = s },
	})
	return mt
}

func backend(id, models string, passing bool) consul.Instance {
	return consul.Instance{ID: id, Passing: passing, Meta: map[string]string{"provider": "openai-compatible", "models": models}}
}

func (mt *modelsTest) pass() error {
	mt.t.Helper()
	return mt.models.Reconcile(context.Background())
}

func (mt *modelsTest) object(model string) *unstructured.Unstructured {
	mt.t.Helper()
	u, err := mt.dyn.Resource(ModelGVR).Get(context.Background(), DerivedName(model, "model"), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return u
}

func (mt *modelsTest) names() []string {
	mt.t.Helper()
	list, err := mt.dyn.Resource(ModelGVR).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		mt.t.Fatal(err)
	}
	var names []string
	for _, item := range list.Items {
		names = append(names, item.GetAnnotations()[AnnotationModel])
	}
	slices.Sort(names)
	return names
}

func TestModelsFromTheCatalog(t *testing.T) {
	mt := newModelsTest(t,
		backend("ollama-1", "qwen3:8b, llama3.2", true),
		backend("ollama-2", "qwen3:8b", false),
		backend("down", "phi4", false),
		consul.Instance{ID: "no-provider", Passing: true, Meta: map[string]string{"models": "skipped"}},
		consul.Instance{ID: "bad-timeout", Passing: true, Meta: map[string]string{"provider": "openai-compatible", "models": "skipped2", "timeout": "5m"}},
		consul.Instance{ID: "bad-path", Passing: true, Meta: map[string]string{"provider": "openai-compatible", "models": "skipped3", "path": "/v1/"}},
	)
	mt.descs.pairs = []consul.Pair{
		{Key: "qwen3:8b", Value: []byte(`{"features": ["TextGeneration"], "contextLength": 32768, "capabilities": ["tools", "json_mode"]}`)},
		{Key: "llama3.2", Value: []byte(`{"capabilities": ["teleport"]}`)},
	}
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	if got := mt.names(); !slices.Equal(got, []string{"llama3.2", "phi4", "qwen3:8b"}) {
		t.Fatalf("objects for %v", got)
	}

	q := mt.object("qwen3:8b")
	spec, _, _ := unstructured.NestedMap(q.Object, "spec")
	if spec["model"] != "qwen3:8b" || spec["endpoint"] != endpoint || spec["contextLength"] != int64(32768) {
		t.Errorf("spec = %v", spec)
	}
	if caps, _, _ := unstructured.NestedStringSlice(q.Object, "spec", "capabilities"); !slices.Equal(caps, []string{"json_mode", "tools"}) {
		t.Errorf("capabilities = %v", caps)
	}
	l := q.GetLabels()
	if l["feature.aisa.hlan.net/TextGeneration"] != "true" || l["capability.aisa.hlan.net/tools"] != "true" ||
		l[LabelManagedBy] != ManagedBy || l[LabelInstall] != "aisa" {
		t.Errorf("labels = %v", l)
	}
	if avail, _, _ := unstructured.NestedBool(q.Object, "status", "available"); !avail {
		t.Error("qwen3:8b has a passing backend and is not available")
	}
	if avail, found, _ := unstructured.NestedBool(mt.object("phi4").Object, "status", "available"); !found || avail {
		t.Error("phi4 has no passing backend and is not marked unavailable")
	}
	// An invalid description: the default one, and counted.
	if feats, _, _ := unstructured.NestedStringSlice(mt.object("llama3.2").Object, "spec", "features"); !slices.Equal(feats, []string{"TextGeneration"}) {
		t.Errorf("llama3.2 features = %v", feats)
	}
	if mt.stats != (ModelStats{Models: 3, Available: 2, InvalidDescriptions: 1}) {
		t.Errorf("stats = %+v", mt.stats)
	}
}

func TestModelsFollowTheCatalog(t *testing.T) {
	mt := newModelsTest(t, backend("ollama-1", "qwen3, phi4", true))
	mt.descs.pairs = []consul.Pair{{Key: "qwen3", Value: []byte(`{"capabilities": ["tools"]}`)}}
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	// phi4 leaves, qwen3's backend fails and its description loses the capability.
	mt.catalog.instances = []consul.Instance{backend("ollama-1", "qwen3", false)}
	mt.descs.pairs = nil
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	if got := mt.names(); !slices.Equal(got, []string{"qwen3"}) {
		t.Fatalf("objects for %v", got)
	}
	q := mt.object("qwen3")
	if _, ok := q.GetLabels()["capability.aisa.hlan.net/tools"]; ok {
		t.Error("the label of a capability the model lost stayed")
	}
	if avail, _, _ := unstructured.NestedBool(q.Object, "status", "available"); avail {
		t.Error("still available with its only backend failing")
	}
}

func TestAnObjectChangedByHandIsRestored(t *testing.T) {
	mt := newModelsTest(t, backend("ollama-1", "qwen3", true))
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	q := mt.object("qwen3")
	_ = unstructured.SetNestedField(q.Object, "http://elsewhere/v1", "spec", "endpoint")
	labels := q.GetLabels()
	labels["team"] = "kept"
	labels["capability.aisa.hlan.net/vision"] = "true"
	q.SetLabels(labels)
	if _, err := mt.dyn.Resource(ModelGVR).Update(context.Background(), q, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	q = mt.object("qwen3")
	if ep, _, _ := unstructured.NestedString(q.Object, "spec", "endpoint"); ep != endpoint {
		t.Errorf("endpoint = %q", ep)
	}
	if l := q.GetLabels(); l["team"] != "kept" || l["capability.aisa.hlan.net/vision"] != "" {
		t.Errorf("labels = %v: want the label aisa does not set kept, and the capability removed", l)
	}
}

func TestModelObjectsThatAreNotThisInstallsAreLeftAlone(t *testing.T) {
	mt := newModelsTest(t, backend("ollama-1", "qwen3", true))
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "aisa.hlan.net/v1alpha1", "kind": "Model",
		"metadata": map[string]any{
			"name":        DerivedName("qwen3", "model"),
			"labels":      map[string]any{LabelManagedBy: ManagedBy, LabelInstall: "staging"},
			"annotations": map[string]any{AnnotationModel: "qwen3"},
		},
		"spec": map[string]any{"model": "qwen3", "endpoint": "http://staging/v1"},
	}}
	if _, err := mt.dyn.Resource(ModelGVR).Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	if ep, _, _ := unstructured.NestedString(mt.object("qwen3").Object, "spec", "endpoint"); ep != "http://staging/v1" {
		t.Error("another install's object was changed")
	}
	if mt.stats.Conflicts != 1 || mt.stats.Models != 0 {
		t.Errorf("stats = %+v", mt.stats)
	}
	// Nor is it deleted when this install no longer offers the model.
	mt.catalog.instances = nil
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	if mt.object("qwen3") == nil {
		t.Error("another install's object was deleted")
	}
}

func TestAnObjectForAnotherModelIsNotTakenOver(t *testing.T) {
	// Two names that derive alike would need a SHA-256 collision; this is the same situation
	// from the other side: an object of the name that this install keeps for another model.
	mt := newModelsTest(t, backend("ollama-1", "qwen3", true))
	clash := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "aisa.hlan.net/v1alpha1", "kind": "Model",
		"metadata": map[string]any{
			"name":        DerivedName("qwen3", "model"),
			"labels":      map[string]any{LabelManagedBy: ManagedBy, LabelInstall: "aisa"},
			"annotations": map[string]any{AnnotationModel: "something-else"},
		},
	}}
	if _, err := mt.dyn.Resource(ModelGVR).Create(context.Background(), clash, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := mt.pass(); err != nil {
			t.Fatal(err)
		}
	}
	if mt.stats.Conflicts != 1 {
		t.Errorf("stats = %+v", mt.stats)
	}
	if o := mt.object("qwen3"); o == nil || o.GetAnnotations()[AnnotationModel] != "something-else" {
		t.Errorf("the object was deleted or taken over: %v", o)
	}
}

func TestModelsKeepTheirObjectsWhenConsulCannotBeRead(t *testing.T) {
	mt := newModelsTest(t, backend("ollama-1", "qwen3", true))
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	mt.catalog.err = errors.New("consul is down")
	if err := mt.pass(); err == nil {
		t.Fatal("want an error")
	}
	mt.catalog.err = nil
	mt.descs.err = errors.New("consul is down")
	if err := mt.pass(); err == nil {
		t.Fatal("want an error")
	}
	if got := mt.names(); !slices.Equal(got, []string{"qwen3"}) {
		t.Errorf("objects for %v after Consul failed", got)
	}
}

func TestAnEmptyCatalogIsWaitedOut(t *testing.T) {
	mt := newModelsTest(t, backend("ollama-1", "qwen3", true))
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	mt.models.opts.EmptyGrace = 5 * time.Minute
	mt.models.opts.Now = func() time.Time { return now }
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	// The catalog comes back empty, as when the token may no longer read the service.
	mt.catalog.instances = nil
	for _, after := range []time.Duration{0, 4 * time.Minute} {
		now = now.Add(after)
		if err := mt.pass(); err != nil {
			t.Fatal(err)
		}
		if got := mt.names(); !slices.Equal(got, []string{"qwen3"}) {
			t.Fatalf("objects for %v within the grace period", got)
		}
	}
	// It comes back: the clock starts again the next time.
	mt.catalog.instances = []consul.Instance{backend("ollama-1", "qwen3", true)}
	now = now.Add(2 * time.Minute)
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	mt.catalog.instances = nil
	now = now.Add(time.Minute)
	if err := mt.pass(); err != nil || len(mt.names()) != 1 {
		t.Fatalf("objects for %v, err %v: the grace period did not start again", mt.names(), err)
	}
	now = now.Add(5 * time.Minute)
	if err := mt.pass(); err != nil {
		t.Fatal(err)
	}
	if got := mt.names(); len(got) != 0 || mt.stats.Models != 0 {
		t.Errorf("objects for %v, stats %+v after the grace period", got, mt.stats)
	}
}

func TestNormalizeKeepsIntegersExact(t *testing.T) {
	if !reflect.DeepEqual(normalize(map[string]any{"contextLength": int64(32768)}), normalize(map[string]any{"contextLength": float64(32768)})) {
		t.Error("the same number from the API server and from here compares as different")
	}
	if reflect.DeepEqual(normalize(map[string]any{"contextLength": int64(9007199254740992)}), normalize(map[string]any{"contextLength": int64(9007199254740993)})) {
		t.Error("two integers above 2^53 compare as equal")
	}
}

func TestParseDescription(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		ok    bool
	}{
		"full":             {`{"features": ["TextEmbedding"], "contextLength": 8192, "capabilities": ["vision"]}`, true},
		"empty":            {`{}`, true},
		"null":             {`null`, false},
		"not json":         {`tools`, false},
		"unknown feature":  {`{"features": ["Dancing"]}`, false},
		"negative context": {`{"contextLength": -1}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := parseDescription([]byte(tc.value))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
			if tc.ok && len(d.Features) == 0 {
				t.Error("no features: want TextGeneration by default")
			}
		})
	}
}

// The backends the proxy's template renders (#91): a path, when there is one, is a prefix.
func TestUsablePath(t *testing.T) {
	for path, want := range map[string]bool{
		"":               true,
		"/v1":            true,
		"/v1beta/openai": true,
		"/openai/v1":     true,
		"/api/v1":        true,
		"/v1/projects/my-project/locations/europe-north1/endpoints/openapi": true,
		"v1":           false,
		"/":            false,
		"/v1/":         false,
		"//v1":         false,
		"/v1//openai":  false,
		"/v1/../admin": false,
		"/./v1":        false,
		"/v1?key=x":    false,
		"/v1 beta":     false,
		"/v1/%2e%2e":   false,
		"/v1...":       true,
	} {
		meta := map[string]string{"provider": "openai-compatible", "models": "m", "path": path}
		if got := usable(meta); got != want {
			t.Errorf("usable with path %q = %v, want %v", path, got, want)
		}
	}
}

// A credential that Vault issues (#92): key_path needs key and key_field, and is a Vault path.
func TestUsableKeyPath(t *testing.T) {
	for _, c := range []struct {
		meta map[string]string
		want bool
	}{
		{map[string]string{}, true},
		{map[string]string{"key": "vertex", "key_path": "gcp/roleset/vertex/token", "key_field": "token"}, true},
		{map[string]string{"key": "vertex", "key_path": "gcp/static-account/vertex/token", "key_field": "access_token"}, true},
		{map[string]string{"key_path": "gcp/roleset/vertex/token", "key_field": "token"}, false},
		{map[string]string{"key": "vertex", "key_path": "gcp/roleset/vertex/token"}, false},
		{map[string]string{"key": "vertex", "key_path": "/gcp/roleset/vertex/token", "key_field": "token"}, false},
		{map[string]string{"key": "vertex", "key_path": "gcp/roleset/vertex/token/", "key_field": "token"}, false},
		{map[string]string{"key": "vertex", "key_path": "gcp/../secret/x", "key_field": "token"}, false},
		{map[string]string{"key": "vertex", "key_path": "gcp/roleset/vertex/token", "key_field": "data.token"}, false},
	} {
		meta := map[string]string{"provider": "openai-compatible", "models": "m"}
		for k, v := range c.meta {
			meta[k] = v
		}
		if got := usable(meta); got != c.want {
			t.Errorf("usable(%v) = %v, want %v", c.meta, got, c.want)
		}
	}
}
