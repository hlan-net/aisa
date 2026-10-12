package ledger

import (
	"container/list"
	"math"
	"sync"
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
	// byPair and order are an LRU of the (consumer, model) pairs: the front of order was
	// observed last, and the back gives way when the table is full.
	byPair  map[pair]*list.Element
	order   *list.List
	byModel map[string]*average
}

type pair struct{ consumer, model string }

type pairEntry struct {
	key pair
	avg average
}

// average is a running mean of the first averageWindow samples and an exponential moving
// average after them.
type average struct {
	mean float64
	n    int
}

func (a *average) add(tokens int64) {
	if a.n < averageWindow {
		a.n++
	}
	a.mean += (float64(tokens) - a.mean) / float64(a.n)
}

// NewEstimator returns an Estimator that answers defaultTokens when it has nothing to go by.
func NewEstimator(defaultTokens int64) *Estimator {
	return &Estimator{
		defaultValue: max(defaultTokens, 0),
		capacity:     estimatorCapacity,
		byPair:       map[pair]*list.Element{},
		order:        list.New(),
		byModel:      map[string]*average{},
	}
}

// Observe learns from an exact event: tokens is its prompt and completion tokens together.
func (e *Estimator) Observe(consumer, model string, tokens int64) {
	if tokens <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	k := pair{consumer, model}
	el, ok := e.byPair[k]
	if ok {
		e.order.MoveToFront(el)
	} else {
		if e.order.Len() >= e.capacity {
			// The pair observed longest ago gives way; its samples still count for the model.
			back := e.order.Back()
			delete(e.byPair, back.Value.(*pairEntry).key)
			e.order.Remove(back)
		}
		el = e.order.PushFront(&pairEntry{key: k})
		e.byPair[k] = el
	}
	el.Value.(*pairEntry).avg.add(tokens)
	m, ok := e.byModel[model]
	if !ok {
		m = &average{}
		e.byModel[model] = m
	}
	m.add(tokens)
}

// Estimate returns the tokens to charge for a request of consumer and model whose usage is
// unknown, and where the figure came from. It is zero, with source "none", only when nothing
// is known and the default is zero.
func (e *Estimator) Estimate(consumer, model string) (int64, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if el, ok := e.byPair[pair{consumer, model}]; ok {
		return round(el.Value.(*pairEntry).avg.mean), estimateFromConsumer
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
// events above zero never becomes a charge of zero. A mean at the top of the range stays
// there: float64(math.MaxInt64) is 2^63, which int64 cannot hold.
func round(mean float64) int64 {
	if mean >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(mean))
}
