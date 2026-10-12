package ledger

import (
	"fmt"
	"math"
	"testing"
)

func TestEstimatorSources(t *testing.T) {
	e := NewEstimator(0)
	if got, src := e.Estimate("chat-ui", "qwen3"); got != 0 || src != estimateNone {
		t.Fatalf("nothing known: got %d from %q, want 0 from none", got, src)
	}

	e = NewEstimator(500)
	if got, src := e.Estimate("chat-ui", "qwen3"); got != 500 || src != estimateFromDefault {
		t.Fatalf("nothing known, with a default: got %d from %q, want 500 from default", got, src)
	}

	e.Observe("batch-jobs", "qwen3", 1000)
	if got, src := e.Estimate("chat-ui", "qwen3"); got != 1000 || src != estimateFromModel {
		t.Fatalf("another consumer's model: got %d from %q, want 1000 from model", got, src)
	}
	if got, src := e.Estimate("chat-ui", "cloud-large"); got != 500 || src != estimateFromDefault {
		t.Fatalf("another model: got %d from %q, want 500 from default", got, src)
	}

	e.Observe("chat-ui", "qwen3", 10)
	if got, src := e.Estimate("chat-ui", "qwen3"); got != 10 || src != estimateFromConsumer {
		t.Fatalf("the consumer's own: got %d from %q, want 10 from consumer", got, src)
	}
	// Both samples count for the model.
	if got, src := e.Estimate("cli", "qwen3"); got != 505 || src != estimateFromModel {
		t.Fatalf("the model's mean: got %d from %q, want 505 from model", got, src)
	}
}

func TestEstimatorAverageRoundsUpAndForgets(t *testing.T) {
	e := NewEstimator(0)
	e.Observe("chat-ui", "qwen3", 1)
	e.Observe("chat-ui", "qwen3", 2)
	// A mean of 1.5 is charged as 2: an estimate errs on the side of the quota.
	if got, _ := e.Estimate("chat-ui", "qwen3"); got != 2 {
		t.Fatalf("mean 1.5: got %d, want 2", got)
	}

	// After the window, the average follows recent samples: 20 of 100 and then 20 of 0 leave
	// well under half of the old level.
	e = NewEstimator(0)
	for range 20 {
		e.Observe("chat-ui", "qwen3", 100)
	}
	for range 20 {
		e.Observe("chat-ui", "qwen3", 1)
	}
	if got, _ := e.Estimate("chat-ui", "qwen3"); got > 40 {
		t.Fatalf("after 20 samples of 1 following 20 of 100: got %d, want well under 50", got)
	}

	// Zero and negative samples are not usage.
	e = NewEstimator(0)
	e.Observe("chat-ui", "qwen3", 0)
	e.Observe("chat-ui", "qwen3", -5)
	if got, src := e.Estimate("chat-ui", "qwen3"); got != 0 || src != estimateNone {
		t.Fatalf("zero samples: got %d from %q, want nothing", got, src)
	}
}

func TestEstimatorCapacity(t *testing.T) {
	e := NewEstimator(0)
	e.capacity = 3
	for i := range 3 {
		e.Observe(fmt.Sprintf("consumer-%d", i), "qwen3", int64(10*(i+1)))
	}
	// consumer-0 is observed again, so consumer-1 is now the pair observed longest ago and
	// gives way to a new one. Its sample still counts for the model.
	e.Observe("consumer-0", "qwen3", 10)
	e.Observe("consumer-3", "qwen3", 40)
	if got, src := e.Estimate("consumer-1", "qwen3"); src != estimateFromModel || got != 22 {
		t.Fatalf("the evicted pair: got %d from %q, want 22 from model", got, src)
	}
	if got, src := e.Estimate("consumer-0", "qwen3"); src != estimateFromConsumer || got != 10 {
		t.Fatalf("the pair observed again: got %d from %q, want 10 from consumer", got, src)
	}
	if got, src := e.Estimate("consumer-2", "qwen3"); src != estimateFromConsumer || got != 30 {
		t.Fatalf("a kept pair: got %d from %q, want 30 from consumer", got, src)
	}
	if len(e.byPair) != 3 || e.order.Len() != 3 {
		t.Fatalf("pairs = %d, order = %d, want 3 and 3", len(e.byPair), e.order.Len())
	}
}

func TestEstimatorAtTheTopOfTheRange(t *testing.T) {
	// AddTokens saturates at MaxInt64, and float64 rounds that to 2^63: the estimate must
	// stay in range rather than overflow to a negative charge.
	e := NewEstimator(0)
	e.Observe("chat-ui", "qwen3", math.MaxInt64)
	if got, _ := e.Estimate("chat-ui", "qwen3"); got != math.MaxInt64 {
		t.Fatalf("a mean at the top of the range: got %d, want MaxInt64", got)
	}
	e.Observe("chat-ui", "qwen3", math.MaxInt64)
	if got, _ := e.Estimate("chat-ui", "qwen3"); got <= 0 {
		t.Fatalf("after two samples at the top of the range: got %d, want a positive charge", got)
	}
}
