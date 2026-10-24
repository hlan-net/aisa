package models

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		ok    bool
	}{
		"full":             {`{"features": ["TextEmbedding"], "contextLength": 8192, "capabilities": ["vision"], "aliases": ["e:latest"]}`, true},
		"empty":            {`{}`, true},
		"null":             {`null`, false},
		"not json":         {`tools`, false},
		"unknown feature":  {`{"features": ["Dancing"]}`, false},
		"unknown cap":      {`{"capabilities": ["flying"]}`, false},
		"negative context": {`{"contextLength": -1}`, false},
		"empty alias":      {`{"aliases": [""]}`, false},
		"padded alias":     {`{"aliases": [" llama3.2:3b"]}`, false},
		"long alias":       {`{"aliases": ["` + strings.Repeat("a", MaxName+1) + `"]}`, false},
		"aliases a string": {`{"aliases": "llama3.2:3b"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Parse([]byte(tc.value))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
			if tc.ok && len(d.Features) == 0 {
				t.Error("no features: want TextGeneration by default")
			}
		})
	}
}

func TestParseSortsAndDeduplicatesAliases(t *testing.T) {
	d, err := Parse([]byte(`{"aliases": ["b", "a", "b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Aliases, []string{"a", "b"}) {
		t.Errorf("aliases = %v", d.Aliases)
	}
}

func pairs(kv ...string) []consul.Pair {
	var out []consul.Pair
	for i := 0; i < len(kv); i += 2 {
		out = append(out, consul.Pair{Key: kv[i], Value: []byte(kv[i+1])})
	}
	return out
}

func TestIndexResolvesAliasesAndRejectsCollisions(t *testing.T) {
	catalog := map[string]bool{"llama3.2": true, "qwen3": true, "gemma3": true}
	ix := NewIndex(pairs(
		"llama3.2", `{"aliases": ["llama3.2:3b", "llama3.2:latest"]}`,
		// An alias that is another model's name.
		"qwen3", `{"aliases": ["gemma3"]}`,
		// Two models claim one alias: neither gets it.
		"gemma3", `{"aliases": ["shared"]}`,
		"phi4-mini", `{"aliases": ["shared"]}`,
		// An alias that is the name of a model known only by its description.
		"mistral", `{"aliases": ["described"]}`,
		"described", `{}`,
		"broken", `not json`,
		"folder/", ``,
	), catalog)

	want := map[string]string{"llama3.2:3b": "llama3.2", "llama3.2:latest": "llama3.2"}
	if !reflect.DeepEqual(ix.Aliases, want) {
		t.Errorf("aliases = %v, want %v", ix.Aliases, want)
	}
	wantInvalid := []string{"broken", "gemma3", "mistral", "phi4-mini", "qwen3"}
	if !reflect.DeepEqual(ix.InvalidNames, wantInvalid) {
		t.Errorf("invalid = %v, want %v", ix.InvalidNames, wantInvalid)
	}
	for _, name := range wantInvalid {
		if _, ok := ix.Descriptions[name]; ok {
			t.Errorf("%s: an invalid description is still among the valid ones", name)
		}
		if got := ix.Of(name); !reflect.DeepEqual(got, Default()) {
			t.Errorf("%s: description = %+v, want the default", name, got)
		}
	}
	if !strings.Contains(ix.Invalid["gemma3"].Error(), "more than one model") {
		t.Errorf("gemma3: %v", ix.Invalid["gemma3"])
	}
	if !strings.Contains(ix.Invalid["qwen3"].Error(), "name of a model") {
		t.Errorf("qwen3: %v", ix.Invalid["qwen3"])
	}
}

func TestNames(t *testing.T) {
	got := Names([]consul.Instance{
		{Meta: map[string]string{"models": "llama3.2, qwen3"}},
		{Meta: map[string]string{"models": " ,qwen3,"}, Passing: false},
		{Meta: map[string]string{}},
	})
	if want := map[string]bool{"llama3.2": true, "qwen3": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
}

type fakeCatalog struct {
	instances []consul.Instance
	err       error
}

func (c *fakeCatalog) Health(context.Context, string) ([]consul.Instance, error) {
	return c.instances, c.err
}

func TestRegistryResolve(t *testing.T) {
	cat := &fakeCatalog{instances: []consul.Instance{{Meta: map[string]string{"models": "llama3.2"}}}}
	r := NewRegistry(cat, nil, slog.New(slog.DiscardHandler), RegistryOptions{})
	ctx := context.Background()

	if got, o := r.Resolve("llama3.2:3b"); got != "llama3.2:3b" || o != NotLoaded {
		t.Errorf("before loading: %q, %v", got, o)
	}
	if r.Ready(ctx) == nil {
		t.Error("ready before loading")
	}
	if !r.Refresh(ctx) {
		t.Fatal("refresh failed")
	}
	if _, o := r.Resolve("llama3.2"); o != NotLoaded {
		t.Errorf("without the descriptions: %v, want NotLoaded", o)
	}
	r.applyDescriptions(pairs(
		"llama3.2", `{"aliases": ["llama3.2:3b"]}`,
		"planned", `{}`,
	))
	if err := r.Ready(ctx); err != nil {
		t.Errorf("ready: %v", err)
	}
	for name, want := range map[string]struct {
		model   string
		outcome Outcome
	}{
		"llama3.2":    {"llama3.2", Model},
		"llama3.2:3b": {"llama3.2", Alias},
		"llama3.2:1b": {"llama3.2:1b", Unknown},
		"planned":     {"planned", Model},
	} {
		if got, o := r.Resolve(name); got != want.model || o != want.outcome {
			t.Errorf("%s: %q, %v; want %q, %v", name, got, o, want.model, want.outcome)
		}
	}

	// Consul cannot be read: what was read stays.
	cat.err = errors.New("down")
	if r.Refresh(ctx) {
		t.Error("refresh succeeded with Consul down")
	}
	if _, o := r.Resolve("llama3.2:3b"); o != Alias {
		t.Errorf("after a failed read: %v, want Alias", o)
	}

	// The model's backends are gone and its description too: the alias goes with it.
	cat.err, cat.instances = nil, nil
	r.Refresh(ctx)
	r.applyDescriptions(nil)
	if _, o := r.Resolve("llama3.2:3b"); o != Unknown {
		t.Errorf("after removal: %v, want Unknown", o)
	}
}

type fakeKV struct{ pairs []consul.Pair }

func (f *fakeKV) List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error) {
	if index > 0 {
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	return f.pairs, 1, nil
}

func TestRegistryRun(t *testing.T) {
	cat := &fakeCatalog{instances: []consul.Instance{{Meta: map[string]string{"models": "llama3.2"}}}}
	kv := &fakeKV{pairs: pairs("llama3.2", `{"aliases": ["llama3.2:3b"]}`)}
	r := NewRegistry(cat, kv, slog.New(slog.DiscardHandler), RegistryOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for r.Ready(ctx) != nil {
		// Run reads both at once; wait for the goroutines.
		select {
		case <-done:
			t.Fatal("Run returned")
		case <-time.After(time.Millisecond):
		}
	}
	if got, o := r.Resolve("llama3.2:3b"); got != "llama3.2" || o != Alias {
		t.Errorf("resolve: %q, %v", got, o)
	}
	cancel()
	<-done
}
