package ledger

import (
	"math"
	"sync"
	"time"
)

// Sources of an estimate, for the debug log.
const (
	estimateFromConsumer = "consumer"
	estimateFromModel    = "model"
	estimateFromDefault  = "default"
	estimateNone         = "none"
)

// averageWindow is the number of samples after which an average turns from a running mean into
// an exponential moving average, so that what a consumer did last week weighs less than today.
const averageWindow = 20

// estimatorCapacity bounds the (consumer, model) pairs an Estimator remembers.
const estimatorCapacity = 10_000

// Estimator guesses the tokens of a successful request whose usage event has none (#12): a
// backend that does not stream usage, or a client that disconnected mid-stream. The guess is
// the recent average of the exact events of the same consumer and model; of the model across
// consumers while the consumer has none; else a default the holder sets, which may be zero.
// Quotas stay conservative with it: such requests are not free.
type Estimator struct {
	mu           sync.Mutex
	defaultValue int64
	capacity     int
	byPair       map[pair]*average
	byModel      map[string]*average
	now          func() time.Time
}

type pair struct{ consumer, model string }

// average is a running mean of the first averageWindow samples and an exponential moving
// average after them.
type average struct {
	mean float64
	n    int
	seen time.Time
}

func (a *average) add(tokens int64, at time.Time) {
	if a.n < averageWindow {
		a.n++
	}
	a.mean += (float64(tokens) - a.mean) / float64(a.n)
	a.seen = at
}

// NewEstimator returns an Estimator that answers defaultTokens when it has nothing to go by.
func NewEstimator(defaultTokens int64) *Estimator {
	return &Estimator{
		defaultValue: max(defaultTokens, 0),
		capacity:     estimatorCapacity,
		byPair:       map[pair]*average{},
		byModel:      map[string]*average{},
		now:          time.Now,
	}
}

// Observe learns from an exact event: tokens is its prompt and completion tokens together.
func (e *Estimator) Observe(consumer, model string, tokens int64) {
	if tokens <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	k := pair{consumer, model}
	a, ok := e.byPair[k]
	if !ok {
		if len(e.byPair) >= e.capacity {
			e.evict()
		}
		a = &average{}
		e.byPair[k] = a
	}
	a.add(tokens, now)
	m, ok := e.byModel[model]
	if !ok {
		m = &average{}
		e.byModel[model] = m
	}
	m.add(tokens, now)
}

// evict forgets the pair that was seen longest ago. The table is full only with thousands of
// consumer and model pairs, so the scan is rare.
func (e *Estimator) evict() {
	var oldest pair
	var found bool
	for k, a := range e.byPair {
		if !found || a.seen.Before(e.byPair[oldest].seen) {
			oldest, found = k, true
		}
	}
	if found {
		delete(e.byPair, oldest)
	}
}

// Estimate returns the tokens to charge for a request of consumer and model whose usage is
// unknown, and where the figure came from. It is zero, with source "none", only when nothing
// is known and the default is zero.
func (e *Estimator) Estimate(consumer, model string) (int64, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a, ok := e.byPair[pair{consumer, model}]; ok {
		return round(a.mean), estimateFromConsumer
	}
	if a, ok := e.byModel[model]; ok {
		return round(a.mean), estimateFromModel
	}
	if e.defaultValue > 0 {
		return e.defaultValue, estimateFromDefault
	}
	return 0, estimateNone
}

// round rounds a mean up: an estimate errs on the side of the quota, and a mean of exact
// events above zero never becomes a charge of zero.
func round(mean float64) int64 {
	return int64(math.Ceil(mean))
}
