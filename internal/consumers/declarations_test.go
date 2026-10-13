package consumers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

func TestParseDeclaration(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  Declaration
		bad   bool
	}{
		"full":            {`{"quota_profile": "batch", "namespace": "news", "secret": "aisa"}`, Declaration{"batch", "news", "aisa"}, false},
		"quota only":      {`{"quota_profile": " interactive "}`, Declaration{QuotaProfile: "interactive"}, false},
		"empty object":    {`{}`, Declaration{}, false},
		"unknown fields":  {`{"quota_profile": "batch", "owner": "team-news"}`, Declaration{QuotaProfile: "batch"}, false},
		"not JSON":        {`batch`, Declaration{}, true},
		"an array":        {`["batch"]`, Declaration{}, true},
		"null":            {` null `, Declaration{}, true},
		"empty":           {``, Declaration{}, true},
		"a number":        {`{"quota_profile": 5}`, Declaration{}, true},
		"namespace alone": {`{"namespace": "news"}`, Declaration{}, true},
		"secret alone":    {`{"secret": "aisa"}`, Declaration{}, true},
		"empty namespace": {`{"namespace": " ", "secret": "aisa"}`, Declaration{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseDeclaration([]byte(tc.value))
			if tc.bad {
				if err == nil {
					t.Fatalf("parsed %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDeclarationsGet(t *testing.T) {
	d := NewDeclarations(nil, quiet(), DeclarationOptions{})
	if _, err := d.Get("chat-ui"); !errors.Is(err, ErrDeclarationsNotLoaded) {
		t.Fatalf("before a load: %v, want ErrDeclarationsNotLoaded", err)
	}
	if err := d.Ready(context.Background()); err == nil {
		t.Fatal("ready before a load")
	}

	var stats []DeclarationStats
	d.opts.OnLoad = func(s DeclarationStats) { stats = append(stats, s) }
	d.apply([]consul.Pair{
		{Key: "", Value: nil},
		{Key: "team/", Value: nil},
		{Key: "chat-ui", Value: []byte(`{"quota_profile": "interactive"}`)},
		{Key: "batch-jobs", Value: []byte(`{}`)},
		{Key: "typo", Value: []byte(`{"quota_profile": }`)},
		{Key: "unknown", Value: []byte(`{}`)},
	})
	if err := d.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Get("chat-ui"); err != nil || got.QuotaProfile != "interactive" {
		t.Fatalf("chat-ui: %+v, %v", got, err)
	}
	if got, err := d.Get("batch-jobs"); err != nil || got != (Declaration{}) {
		t.Fatalf("batch-jobs: %+v, %v; want a declaration without a quota", got, err)
	}
	if _, err := d.Get("typo"); err == nil || errors.Is(err, ErrUndeclared) {
		t.Fatalf("typo: %v, want its own error, not undeclared", err)
	}
	if _, err := d.Get("unknown"); err == nil || errors.Is(err, ErrUndeclared) {
		t.Fatalf("the name unknown: %v, want invalid, not undeclared", err)
	}
	if _, err := d.Get("nobody"); !errors.Is(err, ErrUndeclared) {
		t.Fatalf("nobody: %v, want ErrUndeclared", err)
	}
	if len(stats) != 1 || stats[0].Valid != 2 || stats[0].Invalid != 2 {
		t.Fatalf("stats = %+v, want one load with 2 valid and 2 invalid", stats)
	}
}

// fakeDeclarationLister answers a list of pairs, then blocks until the test changes it.
type fakeDeclarationLister struct {
	mu    sync.Mutex
	pairs []consul.Pair
	index uint64
	fail  error
	calls int
}

func (f *fakeDeclarationLister) List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error) {
	for {
		f.mu.Lock()
		f.calls++
		pairs, idx, fail := f.pairs, f.index, f.fail
		f.mu.Unlock()
		if fail != nil {
			return nil, 0, fail
		}
		if index == 0 || idx != index {
			return pairs, idx, nil
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *fakeDeclarationLister) set(pairs []consul.Pair, index uint64, fail error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs, f.index, f.fail = pairs, index, fail
}

func TestDeclarationsRun(t *testing.T) {
	src := &fakeDeclarationLister{pairs: []consul.Pair{{Key: "chat-ui", Value: []byte(`{"quota_profile": "interactive"}`)}}, index: 7}
	var mu sync.Mutex
	var loads []DeclarationStats
	d := NewDeclarations(src, quiet(), DeclarationOptions{
		RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond,
		OnLoad: func(s DeclarationStats) { mu.Lock(); loads = append(loads, s); mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	wait := func(what string, ok func() bool) {
		t.Helper()
		for range 200 {
			if ok() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s did not happen", what)
	}
	wait("the first load", func() bool { _, err := d.Get("chat-ui"); return err == nil })

	// A change in Consul is followed.
	src.set([]consul.Pair{{Key: "chat-ui", Value: []byte(`{"quota_profile": "batch"}`)}, {Key: "cli", Value: []byte(`{}`)}}, 8, nil)
	wait("the change", func() bool { got, err := d.Get("cli"); return err == nil && got == (Declaration{}) })
	if got, _ := d.Get("chat-ui"); got.QuotaProfile != "batch" {
		t.Fatalf("chat-ui after the change: %+v", got)
	}

	// A Consul that cannot be read keeps the declarations read before.
	src.set(nil, 8, errors.New("consul down"))
	wait("a failed load", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range loads {
			if l.Err != nil {
				return true
			}
		}
		return false
	})
	if _, err := d.Get("cli"); err != nil {
		t.Fatalf("after a failed load: %v, want the declarations read before", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
