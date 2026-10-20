package consul

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Lister reads a prefix of Consul KV. It works like Client.List: with index 0 it answers at
// once, with an earlier index it waits for a change.
type Lister interface {
	List(ctx context.Context, index uint64) ([]Pair, uint64, error)
}

// KVPrefix is a Lister for the keys under a prefix, such as "aisa/consumers/", named by what
// follows the prefix.
type KVPrefix struct {
	Client *Client
	Prefix string
	// What the keys are, for errors: "consumer declarations".
	What string
	// Wait is how long a blocking query waits; 5 minutes when zero.
	Wait time.Duration
}

// List implements Lister.
func (p KVPrefix) List(ctx context.Context, index uint64) ([]Pair, uint64, error) {
	wait := p.Wait
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	pairs, idx, err := p.Client.List(ctx, p.Prefix, index, wait)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s: %w", p.What, err)
	}
	for i := range pairs {
		pairs[i].Key = strings.TrimPrefix(pairs[i].Key, p.Prefix)
	}
	return pairs, idx, nil
}

// FollowOptions configure Follow.
type FollowOptions struct {
	// RetryMin is the wait after a failed read; it doubles with every further failure, up to
	// RetryMax. 1 s and 1 min when zero. It is also the least time between two requests to
	// the source, so a source that answers at once cannot be asked in a tight loop.
	RetryMin, RetryMax time.Duration
}

// Follow reads what src lists, then follows its changes until ctx is done, with blocking
// queries. apply gets every new answer; failed gets every error, after which what was applied
// before stays in use and the next read waits RetryMin, doubling up to RetryMax.
func Follow(ctx context.Context, src Lister, opts FollowOptions, apply func([]Pair), failed func(error)) {
	if opts.RetryMin <= 0 {
		opts.RetryMin = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = time.Minute
	}
	var index uint64
	applied := false
	retry := opts.RetryMin
	for {
		started := time.Now()
		pairs, next, err := src.List(ctx, index)
		if ctx.Err() != nil {
			return
		}
		wait := time.Duration(0)
		if err != nil {
			failed(err)
			wait = retry
			retry = min(retry*2, opts.RetryMax)
		} else {
			retry = opts.RetryMin
			// A blocking query that timed out answers with the same index and nothing new.
			if !applied || next != index {
				apply(pairs)
				applied = true
			}
			index = nextIndex(index, next)
			wait = opts.RetryMin - time.Since(started)
		}
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// nextIndex is the index of the next blocking query, following Consul's advice: an index that
// went backwards, as after a restore of Consul's data, starts over with a query that answers
// at once, and an index is never below 1.
func nextIndex(prev, next uint64) uint64 {
	if next < prev {
		return 0
	}
	return max(next, 1)
}

// ParseEntries parses the values of the entries in pairs, skipping the prefix itself and the
// folders under it. It returns the entries that could be read, those that could not with their
// errors, and the names of the latter in order, for logging.
func ParseEntries[T any](pairs []Pair, parse func([]byte) (T, error)) (valid map[string]T, invalid map[string]error, invalidNames []string) {
	valid = map[string]T{}
	invalid = map[string]error{}
	for _, kv := range pairs {
		if kv.Key == "" || strings.HasSuffix(kv.Key, "/") {
			continue
		}
		v, err := parse(kv.Value)
		if err != nil {
			invalid[kv.Key] = err
			continue
		}
		valid[kv.Key] = v
	}
	invalidNames = make([]string, 0, len(invalid))
	for name := range invalid {
		invalidNames = append(invalidNames, name)
	}
	sort.Strings(invalidNames)
	return valid, invalid, invalidNames
}
