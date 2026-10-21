package pricing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hlan-net/aisa/internal/consul"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParsePrice(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  Price
		bad   bool
	}{
		"paid model":        {`{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "EUR"}`, Price{0.003, 0.015, "EUR"}, false},
		"free model":        {`{"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": " EUR "}`, Price{0, 0, "EUR"}, false},
		"unknown fields":    {`{"input_per_1k": 0.01, "output_per_1k": 0.02, "currency": "USD", "note": "v1"}`, Price{0.01, 0.02, "USD"}, false},
		"not JSON":          {`free`, Price{}, true},
		"an array":          {`[0.003]`, Price{}, true},
		"null":              {` null `, Price{}, true},
		"empty":             {``, Price{}, true},
		"empty object":      {`{}`, Price{}, true},
		"missing input":     {`{"output_per_1k": 0.015, "currency": "EUR"}`, Price{}, true},
		"missing output":    {`{"input_per_1k": 0.003, "currency": "EUR"}`, Price{}, true},
		"missing currency":  {`{"input_per_1k": 0.003, "output_per_1k": 0.015}`, Price{}, true},
		"empty currency":    {`{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "  "}`, Price{}, true},
		"long currency":     {`{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "` + strings.Repeat("E", 257) + `"}`, Price{}, true},
		"negative input":    {`{"input_per_1k": -0.001, "output_per_1k": 0.015, "currency": "EUR"}`, Price{}, true},
		"negative output":   {`{"input_per_1k": 0.003, "output_per_1k": -0.015, "currency": "EUR"}`, Price{}, true},
		"string for number": {`{"input_per_1k": "0.003", "output_per_1k": 0.015, "currency": "EUR"}`, Price{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePrice([]byte(tc.value))
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

func TestPriceCostAndPaid(t *testing.T) {
	paid := Price{InputPer1K: 0.003, OutputPer1K: 0.015, Currency: "EUR"}
	if !paid.Paid() {
		t.Fatal("want cloud-large to be paid")
	}
	// 1 000 prompt (0.003) + 2 000 completion (0.030) = 0.033.
	if got := paid.Cost(1000, 2000); math.Abs(got-0.033) > 1e-12 {
		t.Fatalf("Cost(1000, 2000) = %v, want 0.033", got)
	}
	if got := paid.Cost(-100, 0); got != 0 {
		t.Fatalf("Cost(-100, 0) = %v, want 0", got)
	}

	free := Price{InputPer1K: 0, OutputPer1K: 0, Currency: "EUR"}
	if free.Paid() {
		t.Fatal("want 0.0 model to be free")
	}
	if got := free.Cost(5000, 5000); got != 0 {
		t.Fatalf("free Cost = %v, want 0", got)
	}
}

func TestPricesGetAndLookup(t *testing.T) {
	p := New(nil, quiet(), Options{})
	if _, err := p.Get("cloud-large"); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("before a load: %v, want ErrNotLoaded", err)
	}
	if _, ok := p.Lookup("cloud-large"); ok {
		t.Fatal("Lookup before a load succeeded")
	}
	if err := p.Ready(context.Background()); err == nil {
		t.Fatal("ready before a load")
	}

	var stats []LoadStats
	p.opts.OnLoad = func(s LoadStats) { stats = append(stats, s) }
	p.apply([]consul.Pair{
		{Key: "", Value: nil},
		{Key: "sub/", Value: nil},
		{Key: "cloud-large", Value: []byte(`{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "EUR"}`)},
		{Key: "qwen3", Value: []byte(`{"input_per_1k": 0.0, "output_per_1k": 0.0, "currency": "EUR"}`)},
		{Key: "broken", Value: []byte(`{"input_per_1k": -1}`)},
	})
	if err := p.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Get("cloud-large"); err != nil || got.Currency != "EUR" || !got.Paid() {
		t.Fatalf("cloud-large: %+v, %v", got, err)
	}
	if got, ok := p.Lookup("qwen3"); !ok || got.Paid() {
		t.Fatalf("Lookup(qwen3) = %+v, %v", got, ok)
	}
	if _, err := p.Get("broken"); err == nil || errors.Is(err, ErrNoPrice) {
		t.Fatalf("broken: %v, want its own error, not ErrNoPrice", err)
	}
	if _, ok := p.Lookup("broken"); ok {
		t.Fatal("Lookup(broken) succeeded")
	}
	if _, err := p.Get("unpriced"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("unpriced: %v, want ErrNoPrice", err)
	}
	if len(stats) != 1 || stats[0].Valid != 2 || stats[0].Invalid != 1 {
		t.Fatalf("stats = %+v, want one load with 2 valid and 1 invalid", stats)
	}
}

type fakePriceLister struct {
	mu    sync.Mutex
	pairs []consul.Pair
	index uint64
	fail  error
}

func (f *fakePriceLister) List(ctx context.Context, index uint64) ([]consul.Pair, uint64, error) {
	for {
		f.mu.Lock()
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

func (f *fakePriceLister) set(pairs []consul.Pair, index uint64, fail error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairs, f.index, f.fail = pairs, index, fail
}

func TestPricesRun(t *testing.T) {
	src := &fakePriceLister{
		pairs: []consul.Pair{{Key: "cloud-large", Value: []byte(`{"input_per_1k": 0.003, "output_per_1k": 0.015, "currency": "EUR"}`)}},
		index: 3,
	}
	var mu sync.Mutex
	var loads []LoadStats
	p := New(src, quiet(), Options{
		RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond,
		OnLoad: func(s LoadStats) { mu.Lock(); loads = append(loads, s); mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

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
	wait("the first load", func() bool { _, ok := p.Lookup("cloud-large"); return ok })

	// A change in Consul is followed.
	src.set([]consul.Pair{
		{Key: "cloud-large", Value: []byte(`{"input_per_1k": 0.005, "output_per_1k": 0.020, "currency": "EUR"}`)},
		{Key: "qwen3", Value: []byte(`{"input_per_1k": 0, "output_per_1k": 0, "currency": "EUR"}`)},
	}, 4, nil)
	wait("the change", func() bool { _, ok := p.Lookup("qwen3"); return ok })
	if got, _ := p.Lookup("cloud-large"); got.InputPer1K != 0.005 {
		t.Fatalf("cloud-large after the change: %+v", got)
	}

	// A Consul that cannot be read keeps the prices read before.
	src.set(nil, 4, errors.New("consul down"))
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
	if _, ok := p.Lookup("qwen3"); !ok {
		t.Fatal("after a failed load: want the prices read before")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
