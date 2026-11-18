package models

import (
	"maps"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultModelSource returns a map of owner name (consumer or quota profile) to its
// configured default_model, and whether that source has completed its first load.
type DefaultModelSource func() (map[string]string, bool)

// UnknownDefaultModels returns the entries from src whose configured default_model matches
// neither a model nor an alias in the registry. Before either the registry (catalog and
// descriptions) or src has loaded, it returns nil so startup does not report false positives.
func (r *Registry) UnknownDefaultModels(src DefaultModelSource) map[string]string {
	if r == nil || src == nil {
		return nil
	}
	configured, loaded := src()
	if !loaded || len(configured) == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.catalogRead || !r.descsRead {
		return nil
	}
	var unknown map[string]string
	for owner, model := range configured {
		if model == "" {
			continue
		}
		if _, outcome := r.resolveLocked(model); outcome == Unknown {
			if unknown == nil {
				unknown = make(map[string]string)
			}
			unknown[owner] = model
		}
	}
	return unknown
}

// DefaultModelCollectors returns the aisa_default_models_unknown{source} gauges for consumer
// declarations (source="consumer") and quota profiles (source="profile"). Computed at scrape
// time so changes in Consul catalog, model descriptions, consumers, or profiles appear at once.
func (r *Registry) DefaultModelCollectors(consumers, profiles DefaultModelSource) []prometheus.Collector {
	gauge := func(source string, src DefaultModelSource) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "aisa_default_models_unknown",
			Help:        "Configured default_model values that match no known model or alias in Consul, by source (consumer or profile).",
			ConstLabels: prometheus.Labels{"source": source},
		}, func() float64 {
			return float64(len(r.UnknownDefaultModels(src)))
		})
	}
	return []prometheus.Collector{
		gauge("consumer", consumers),
		gauge("profile", profiles),
	}
}

// logUnknownDefaultModels logs newly-unknown or changed unknown default_model entries once.
func (r *Registry) logUnknownDefaultModels() {
	if r.opts.DefaultModelConsumers == nil && r.opts.DefaultModelProfiles == nil {
		return
	}
	current := map[string]string{}
	recordSource := func(source string, src DefaultModelSource) {
		unknown := r.UnknownDefaultModels(src)
		for _, owner := range slices.Sorted(maps.Keys(unknown)) {
			model := unknown[owner]
			key := source + ":" + owner
			current[key] = model
			if r.reportedUnknown[key] != model {
				r.log.Warn("default_model matches no known model or alias in Consul",
					"source", source, "name", owner, "default_model", model)
			}
		}
	}
	r.unknownMu.Lock()
	defer r.unknownMu.Unlock()
	recordSource("consumer", r.opts.DefaultModelConsumers)
	recordSource("profile", r.opts.DefaultModelProfiles)
	r.reportedUnknown = current
}
