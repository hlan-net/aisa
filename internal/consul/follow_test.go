package consul

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// answer is one reply of a scripted Lister.
type answer struct {
	pairs []Pair
	index uint64
	err   error
}

// scripted answers List from its script, records the index of each call, and cancels the
// context once the script is over.
type scripted struct {
	mu      sync.Mutex
	script  []answer
	indexes []uint64
	cancel  context.CancelFunc
}

func (s *scripted) List(ctx context.Context, index uint64) ([]Pair, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexes = append(s.indexes, index)
	if len(s.script) == 0 {
		s.cancel()
		return nil, 0, ctx.Err()
	}
	a := s.script[0]
	s.script = s.script[1:]
	return a.pairs, a.index, a.err
}

func TestFollow(t *testing.T) {
	a, b, c := []Pair{{Key: "a"}}, []Pair{{Key: "b"}}, []Pair{{Key: "c"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &scripted{cancel: cancel, script: []answer{
		{pairs: a, index: 5},
		{pairs: a, index: 5}, // a blocking query that timed out: nothing to apply
		{err: errors.New("consul down")},
		{pairs: b, index: 9},
		{pairs: c, index: 3}, // the index went backwards: the next query starts over
	}}
	var applied []string
	var failures int
	done := make(chan struct{})
	go func() {
		defer close(done)
		Follow(ctx, src, FollowOptions{RetryMin: time.Millisecond, RetryMax: 2 * time.Millisecond},
			func(p []Pair) { applied = append(applied, p[0].Key) },
			func(error) { failures++ })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return after its context was cancelled")
	}

	if got := strings.Join(applied, ","); got != "a,b,c" {
		t.Errorf("applied %q, want a,b,c", got)
	}
	if failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
	// After the error the index stays; after the index went back to 3 the query starts at 0.
	want := []uint64{0, 5, 5, 5, 9, 0}
	if len(src.indexes) != len(want) {
		t.Fatalf("indexes %v, want %v", src.indexes, want)
	}
	for i := range want {
		if src.indexes[i] != want[i] {
			t.Fatalf("indexes %v, want %v", src.indexes, want)
		}
	}
}

func TestFollowStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &scripted{cancel: func() {}, script: []answer{{err: errors.New("consul down")}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Zero options: a retry of a second, during which the context is cancelled.
		Follow(ctx, src, FollowOptions{}, func([]Pair) {}, func(error) { cancel() })
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Follow did not stop while it waited to retry")
	}
}

func TestNextIndex(t *testing.T) {
	for _, tc := range []struct{ prev, next, want uint64 }{
		{0, 0, 1},
		{0, 7, 7},
		{7, 7, 7},
		{7, 9, 9},
		{9, 3, 0},
	} {
		if got := nextIndex(tc.prev, tc.next); got != tc.want {
			t.Errorf("nextIndex(%d, %d) = %d, want %d", tc.prev, tc.next, got, tc.want)
		}
	}
}

func TestKVPrefixList(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Consul-Index", "12")
		_, _ = w.Write([]byte(`[{"Key":"aisa/consumers/chat-ui","Value":"e30="}]`))
	}))
	defer srv.Close()
	c, _ := New(Options{Addr: srv.URL})

	pairs, idx, err := KVPrefix{Client: c, Prefix: "aisa/consumers/", What: "consumer declarations"}.List(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 12 || len(pairs) != 1 || pairs[0].Key != "chat-ui" || string(pairs[0].Value) != "{}" {
		t.Errorf("got %+v at %d, want chat-ui at 12", pairs, idx)
	}
	if !strings.Contains(gotQuery, "wait=300s") {
		t.Errorf("query %q, want the default wait of 5 minutes", gotQuery)
	}

	srv.Close()
	_, _, err = KVPrefix{Client: c, Prefix: "aisa/consumers/", What: "consumer declarations"}.List(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "list consumer declarations") {
		t.Errorf("err = %v, want one that names the consumer declarations", err)
	}
}

func TestParseEntries(t *testing.T) {
	parse := func(v []byte) (string, error) {
		if len(v) == 0 {
			return "", errors.New("empty")
		}
		return string(v), nil
	}
	valid, invalid, names := ParseEntries([]Pair{
		{Key: "", Value: []byte("prefix")},
		{Key: "folder/", Value: nil},
		{Key: "b"},
		{Key: "ok", Value: []byte("v")},
		{Key: "a"},
	}, parse)
	if len(valid) != 1 || valid["ok"] != "v" {
		t.Errorf("valid = %v, want only ok=v", valid)
	}
	if len(invalid) != 2 || invalid["a"] == nil || invalid["b"] == nil {
		t.Errorf("invalid = %v, want a and b", invalid)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("names = %v, want [a b]", names)
	}
}
