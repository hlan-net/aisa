package models

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hlan-net/aisa/internal/consul"
)

type fakeHealthReader struct {
	instances []consul.Instance
}

func (f fakeHealthReader) Health(context.Context, string) ([]consul.Instance, error) {
	return f.instances, nil
}

func TestUnknownDefaultModels(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := fakeHealthReader{
		instances: []consul.Instance{
			{Meta: map[string]string{"models": "qwen3"}},
		},
	}
	consumerModels := map[string]string{
		"chat-ui": "qwen3",
		"cli":     "qwen3:8b",
		"typo":    "nonexistent",
	}
	profileModels := map[string]string{
		"interactive": "qwen3",
		"broken":      "missing-model",
	}
	consumersLoaded := false
	profilesLoaded := false

	r := NewRegistry(catalog, nil, discard, RegistryOptions{
		DefaultModelConsumers: func() (map[string]string, bool) { return consumerModels, consumersLoaded },
		DefaultModelProfiles:  func() (map[string]string, bool) { return profileModels, profilesLoaded },
	})

	// Before catalog/descriptions/sources load, nothing is reported as unknown.
	if got := r.UnknownDefaultModels(r.opts.DefaultModelConsumers); len(got) != 0 {
		t.Fatalf("before load: got %v, want empty", got)
	}

	r.Refresh(context.Background())
	r.applyDescriptions([]consul.Pair{
		{Key: "qwen3", Value: []byte(`{"aliases": ["qwen3:8b"]}`)},
	})

	// Sources still not loaded.
	if got := r.UnknownDefaultModels(r.opts.DefaultModelConsumers); len(got) != 0 {
		t.Fatalf("before source load: got %v, want empty", got)
	}

	consumersLoaded = true
	profilesLoaded = true

	gotConsumers := r.UnknownDefaultModels(r.opts.DefaultModelConsumers)
	wantConsumers := map[string]string{"typo": "nonexistent"}
	if !maps.Equal(gotConsumers, wantConsumers) {
		t.Errorf("consumers unknown = %v, want %v", gotConsumers, wantConsumers)
	}

	gotProfiles := r.UnknownDefaultModels(r.opts.DefaultModelProfiles)
	wantProfiles := map[string]string{"broken": "missing-model"}
	if !maps.Equal(gotProfiles, wantProfiles) {
		t.Errorf("profiles unknown = %v, want %v", gotProfiles, wantProfiles)
	}

	// Verify Prometheus collectors report the counts by source.
	promReg := prometheus.NewRegistry()
	promReg.MustRegister(r.DefaultModelCollectors(r.opts.DefaultModelConsumers, r.opts.DefaultModelProfiles)...)
	wantMetrics := `
# HELP aisa_default_models_unknown Configured default_model values that match no known model or alias in Consul, by source (consumer or profile).
# TYPE aisa_default_models_unknown gauge
aisa_default_models_unknown{source="consumer"} 1
aisa_default_models_unknown{source="profile"} 1
`
	if err := testutil.GatherAndCompare(promReg, strings.NewReader(wantMetrics), "aisa_default_models_unknown"); err != nil {
		t.Fatalf("GatherAndCompare: %v", err)
	}

	// Fixing the profile model clears it on the next scrape.
	profileModels["broken"] = "qwen3:8b"
	consumerModels["empty"] = ""
	r.logUnknownDefaultModels()
	if got := r.UnknownDefaultModels(r.opts.DefaultModelProfiles); len(got) != 0 {
		t.Errorf("after fix: profiles unknown = %v, want empty", got)
	}

	// Nil receiver or nil source returns nil safely.
	var nilReg *Registry
	if got := nilReg.UnknownDefaultModels(r.opts.DefaultModelConsumers); got != nil {
		t.Errorf("nil Registry.UnknownDefaultModels = %v, want nil", got)
	}
	if got := r.UnknownDefaultModels(nil); got != nil {
		t.Errorf("nil source UnknownDefaultModels = %v, want nil", got)
	}
}
