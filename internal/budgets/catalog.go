package budgets

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

// HealthReader reads the backends registered in the catalog; consul.Client implements it.
type HealthReader interface {
	Health(ctx context.Context, service string) ([]consul.Instance, error)
}

// CatalogOptions configure Catalog.
type CatalogOptions struct {
	// Service is the backends' service name in the catalog, "aisa-backend".
	Service string
	// Refresh is how often the catalog is read again; 30 s when zero.
	Refresh time.Duration
}

// Catalog is what the budgets need of the backends in Consul: which provider keys serve a
// model, and the key of a backend. A backend names its key in the service meta key
// (docs/concepts/CONSUL.md); a backend without one is a local model, which no provider budget
// bounds.
type Catalog struct {
	reader HealthReader
	log    *slog.Logger
	opts   CatalogOptions

	mu     sync.RWMutex
	loaded bool
	// byModel maps a model to the sorted, distinct keys of the backends that serve it; a model
	// served by a keyless backend too is still listed under its keys.
	byModel map[string][]string
	// byBackend maps a backend's ID, address and address:port to its key.
	byBackend map[string]string
}

// NewCatalog returns an empty catalog. Call Run to fill it and keep it current.
func NewCatalog(r HealthReader, log *slog.Logger, opts CatalogOptions) *Catalog {
	if opts.Service == "" {
		opts.Service = "aisa-backend"
	}
	if opts.Refresh <= 0 {
		opts.Refresh = 30 * time.Second
	}
	return &Catalog{reader: r, log: log, opts: opts, byModel: map[string][]string{}, byBackend: map[string]string{}}
}

// Run reads the catalog, then again every Refresh until ctx is done. When Consul cannot be
// read, what was read before stays in use.
func (c *Catalog) Run(ctx context.Context) {
	c.Refresh(ctx)
	t := time.NewTicker(c.opts.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Refresh(ctx)
		}
	}
}

// Refresh reads the catalog once and returns whether it could.
func (c *Catalog) Refresh(ctx context.Context) bool {
	instances, err := c.reader.Health(ctx, c.opts.Service)
	if err != nil {
		c.log.Error("cannot read the backends for the provider budgets; keeping those read before", "error", err)
		return false
	}
	c.apply(instances)
	return true
}

// apply replaces the catalog with the instances read. Every registration counts, passing or
// not: a backend that is down keeps its key, and a request may be sent to it as soon as it is
// back.
func (c *Catalog) apply(instances []consul.Instance) {
	byModel := map[string][]string{}
	byBackend := map[string]string{}
	// A name that two backends with different keys share, such as an address with several
	// ports, names neither: a charge by it would go to whichever came last.
	ambiguous := map[string]bool{}
	for _, in := range instances {
		key := strings.TrimSpace(in.Meta["key"])
		if key == "" {
			continue
		}
		for _, name := range []string{in.ID, in.Address, net.JoinHostPort(in.Address, strconv.Itoa(in.Port))} {
			if name == "" {
				continue
			}
			if other, ok := byBackend[name]; ok && other != key {
				ambiguous[name] = true
			}
			byBackend[name] = key
		}
		for model := range strings.SplitSeq(in.Meta["models"], ",") {
			model = strings.TrimSpace(model)
			if model == "" || slices.Contains(byModel[model], key) {
				continue
			}
			byModel[model] = append(byModel[model], key)
		}
	}
	for _, keys := range byModel {
		sort.Strings(keys)
	}
	for name := range ambiguous {
		delete(byBackend, name)
	}
	c.mu.Lock()
	c.loaded, c.byModel, c.byBackend = true, byModel, byBackend
	c.mu.Unlock()
}

// Loaded reports whether the catalog has been read.
func (c *Catalog) Loaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loaded
}

// KeysOf returns the provider keys of the backends that serve a model, none for a model that
// only local backends serve, or that is not in the catalog.
func (c *Catalog) KeysOf(model string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.byModel[model])
}

// KeyOf returns the provider key of a backend, named by its ID, address or address:port, as a
// usage event may name it.
func (c *Catalog) KeyOf(backend string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key, ok := c.byBackend[strings.TrimSpace(backend)]
	return key, ok
}
