package consumers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
	"github.com/hlan-net/aisa/internal/metrics"
)

// Declaration is the part of a consumer that is not a secret, aisa/consumers/<name> in Consul
// KV (docs/concepts/CONSUL.md): the holder declares a consumer there, and Vault holds only the
// hashes of its keys. A key whose consumer is not declared does not authenticate.
type Declaration struct {
	// QuotaProfile names the consumer's quota profile, aisa/quotas/<profile>; empty: no quota.
	QuotaProfile string
	// Namespace and Secret say where the consumer's application runs and the Secret aisa is to
	// write its key into (docs/concepts/KUBERNETES_OBJECTS.md); both or neither.
	Namespace, Secret string
}

// Declared answers which consumers are declared; Declarations implements it.
type Declared interface {
	// Get returns a consumer's declaration, ErrDeclarationsNotLoaded before the first read,
	// ErrUndeclared for a consumer that is not declared, and another error for a declaration
	// that cannot be read.
	Get(name string) (Declaration, error)
}

// Errors of Declarations.Get.
var (
	// ErrDeclarationsNotLoaded: the declarations have not been read yet.
	ErrDeclarationsNotLoaded = errors.New("consumer declarations not loaded yet")
	// ErrUndeclared: no declaration has the name.
	ErrUndeclared = errors.New("consumer is not declared")
)

// DeclarationStats describe a load of the declarations, for metrics.
type DeclarationStats struct {
	// Err is nil when the load replaced the declarations.
	Err error
	// Valid and Invalid count the declarations that could and could not be read.
	Valid, Invalid int
}

// DeclarationOptions configure Declarations.
type DeclarationOptions struct {
	// RetryMin and RetryMax are the waits after a failed read; see consul.FollowOptions.
	RetryMin, RetryMax time.Duration
	// OnLoad is called after every load, when set.
	OnLoad func(DeclarationStats)
}

// Declarations holds the consumers declared in Consul, kept up to date with the source.
type Declarations struct {
	src  consul.Lister
	opts DeclarationOptions
	log  *slog.Logger

	mu      sync.RWMutex
	loaded  bool
	valid   map[string]Declaration
	invalid map[string]error
}

// NewDeclarations returns an empty set of declarations. Call Run to fill it.
func NewDeclarations(src consul.Lister, log *slog.Logger, opts DeclarationOptions) *Declarations {
	return &Declarations{src: src, opts: opts, log: log}
}

// Get implements Declared. A declaration that exists but cannot be read returns its error, so a
// typo does not pass for a consumer without a quota.
func (d *Declarations) Get(name string) (Declaration, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.loaded {
		return Declaration{}, ErrDeclarationsNotLoaded
	}
	if decl, ok := d.valid[name]; ok {
		return decl, nil
	}
	if err, ok := d.invalid[name]; ok {
		return Declaration{}, fmt.Errorf("declaration of consumer %q: %w", name, err)
	}
	return Declaration{}, fmt.Errorf("%w: %q", ErrUndeclared, name)
}

// Ready returns nil once the declarations have been read, for aisa's readiness.
func (d *Declarations) Ready(context.Context) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.loaded {
		return ErrDeclarationsNotLoaded
	}
	return nil
}

// Run reads the declarations, then follows their changes until ctx is done. When the source
// cannot be read, the declarations read before stay in use.
func (d *Declarations) Run(ctx context.Context) {
	consul.Follow(ctx, d.src, consul.FollowOptions{RetryMin: d.opts.RetryMin, RetryMax: d.opts.RetryMax}, d.apply, func(err error) {
		d.log.Error("reading consumer declarations failed; keeping the ones read before", "error", err)
		if d.opts.OnLoad != nil {
			d.opts.OnLoad(DeclarationStats{Err: err})
		}
	})
}

// apply replaces the declarations with those read.
func (d *Declarations) apply(pairs []consul.Pair) {
	valid := map[string]Declaration{}
	invalid := map[string]error{}
	for _, kv := range pairs {
		// The prefix itself, or a folder under it.
		if kv.Key == "" || strings.HasSuffix(kv.Key, "/") {
			continue
		}
		if kv.Key == metrics.ConsumerUnknown {
			// The label of requests without a known consumer; a consumer of that name would be
			// counted with them.
			invalid[kv.Key] = errors.New("a consumer may not be named " + metrics.ConsumerUnknown)
			continue
		}
		decl, err := parseDeclaration(kv.Value)
		if err != nil {
			invalid[kv.Key] = err
			continue
		}
		valid[kv.Key] = decl
	}
	names := make([]string, 0, len(invalid))
	for name := range invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// The consumer's keys are answered 503 until it is fixed.
		d.log.Error("invalid consumer declaration", "consumer", name, "error", invalid[name])
	}

	d.mu.Lock()
	d.loaded, d.valid, d.invalid = true, valid, invalid
	d.mu.Unlock()
	d.log.Info("consumer declarations loaded", "valid", len(valid), "invalid", len(invalid))
	if d.opts.OnLoad != nil {
		d.opts.OnLoad(DeclarationStats{Valid: len(valid), Invalid: len(invalid)})
	}
}

// parseDeclaration reads a declaration's JSON value, such as
// {"quota_profile": "batch", "namespace": "news", "secret": "aisa"}. Every field is optional,
// except that namespace and secret come together; fields it does not know are ignored, so a
// later version can add some. An empty object declares a consumer without a quota.
func parseDeclaration(value []byte) (Declaration, error) {
	var raw struct {
		QuotaProfile *string `json:"quota_profile"`
		Namespace    *string `json:"namespace"`
		Secret       *string `json:"secret"`
	}
	// json.Unmarshal takes null as well, and leaves raw empty: a declaration without a quota.
	if trimmed := bytes.TrimSpace(value); len(trimmed) == 0 || trimmed[0] != '{' {
		return Declaration{}, errors.New("want a JSON object")
	}
	if err := json.Unmarshal(value, &raw); err != nil {
		return Declaration{}, fmt.Errorf("want a JSON object: %w", err)
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	decl := Declaration{QuotaProfile: str(raw.QuotaProfile), Namespace: str(raw.Namespace), Secret: str(raw.Secret)}
	if (decl.Namespace == "") != (decl.Secret == "") {
		return Declaration{}, errors.New("namespace and secret come together")
	}
	return decl, nil
}
