package models

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

// HealthReader reads the backends registered in the catalog; consul.Client implements it.
type HealthReader interface {
	Health(ctx context.Context, service string) ([]consul.Instance, error)
}

// RegistryOptions configure Registry.
type RegistryOptions struct {
	// Service is the backends' service name in the catalog, "aisa-backend" when empty.
	Service string
	// Refresh is how often the catalog is read again; 30 s when zero. The descriptions are
	// followed with blocking queries.
	Refresh time.Duration
	// RetryMin and RetryMax are the waits after a failed read of the descriptions; see
	// consul.FollowOptions.
	RetryMin, RetryMax time.Duration
}

// Outcome is what Resolve found for a name.
type Outcome int

const (
	// NotLoaded: the catalog or the descriptions have not been read yet; the name is let
	// through as it is.
	NotLoaded Outcome = iota
	// Model: the name is a model's.
	Model
	// Alias: the name is an alias of a model.
	Alias
	// Unknown: the name is neither.
	Unknown
)

// Registry keeps the names of the models, from the catalog and the descriptions, for the
// decision: which name is a model's, and which model an alias stands for.
type Registry struct {
	catalog HealthReader
	descs   consul.Lister
	log     *slog.Logger
	opts    RegistryOptions

	mu          sync.RWMutex
	names       map[string]bool
	pairs       []consul.Pair
	catalogRead bool
	descsRead   bool
	index       Index
	// reported is the invalid descriptions last logged, so a change is logged once.
	reported map[string]string
}

// NewRegistry returns an empty registry. Call Run to fill it and keep it current.
func NewRegistry(catalog HealthReader, descs consul.Lister, log *slog.Logger, opts RegistryOptions) *Registry {
	if opts.Service == "" {
		opts.Service = "aisa-backend"
	}
	if opts.Refresh <= 0 {
		opts.Refresh = 30 * time.Second
	}
	return &Registry{catalog: catalog, descs: descs, log: log, opts: opts, names: map[string]bool{}, reported: map[string]string{}}
}

// Run reads the catalog and the descriptions, and keeps them current until ctx is done. When
// Consul cannot be read, what was read before stays in use.
func (r *Registry) Run(ctx context.Context) {
	go consul.Follow(ctx, r.descs, consul.FollowOptions{RetryMin: r.opts.RetryMin, RetryMax: r.opts.RetryMax},
		r.applyDescriptions,
		func(err error) {
			r.log.Error("cannot read the model descriptions; keeping those read before", "error", err)
		})
	r.Refresh(ctx)
	t := time.NewTicker(r.opts.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Refresh(ctx)
		}
	}
}

// Refresh reads the catalog once and returns whether it could.
func (r *Registry) Refresh(ctx context.Context) bool {
	instances, err := r.catalog.Health(ctx, r.opts.Service)
	if err != nil {
		r.log.Error("cannot read the backends for the model names; keeping those read before", "error", err)
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names, r.catalogRead = Names(instances), true
	r.reindex()
	return true
}

func (r *Registry) applyDescriptions(pairs []consul.Pair) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairs, r.descsRead = pairs, true
	r.reindex()
}

// reindex reads the descriptions against the catalog's names, with r.mu held, and logs the
// descriptions that became invalid or changed why.
func (r *Registry) reindex() {
	r.index = NewIndex(r.pairs, r.names)
	now := make(map[string]string, len(r.index.Invalid))
	for _, model := range r.index.InvalidNames {
		why := r.index.Invalid[model].Error()
		now[model] = why
		if r.reported[model] != why {
			r.log.Error("invalid model description; its aliases are not resolved", "model", model, "error", why)
		}
	}
	r.reported = now
}

// Resolve returns the model a requested name stands for and what the name is: the name
// itself for a model, its model for an alias. A name the registry cannot place yet, or does
// not know, is returned as it is.
func (r *Registry) Resolve(name string) (string, Outcome) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.catalogRead || !r.descsRead {
		return name, NotLoaded
	}
	if model, ok := r.index.Aliases[name]; ok {
		return model, Alias
	}
	if r.names[name] || r.hasDescription(name) {
		return name, Model
	}
	return name, Unknown
}

// hasDescription reports whether a description of the name is in Consul, readable or not:
// the holder declared the model, whether a backend serves it yet or not. Called with r.mu held.
func (r *Registry) hasDescription(name string) bool {
	if _, ok := r.index.Descriptions[name]; ok {
		return true
	}
	_, ok := r.index.Invalid[name]
	return ok
}

var errNotLoaded = errors.New("the model names have not been read yet")

// Ready returns nil once the catalog and the descriptions have been read.
func (r *Registry) Ready(context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.catalogRead || !r.descsRead {
		return errNotLoaded
	}
	return nil
}
