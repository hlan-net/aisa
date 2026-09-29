package quotas

import (
	"context"
	"log/slog"
	"time"

	"github.com/hlan-net/aisa/internal/consumers"
	"github.com/hlan-net/aisa/internal/metrics"
)

// Counter keeps the tokens of the consumers; Window implements it.
type Counter interface {
	Add(ctx context.Context, consumer string, tokens int64, at time.Time) error
	Usage(ctx context.Context, consumer string, now time.Time) ([]int64, error)
}

// Outcome of a quota check.
type Outcome int

const (
	// Allow: the consumer has tokens left, has no quota profile, or its usage could not be
	// read.
	Allow Outcome = iota
	// Exhausted: the consumer has used its tokens for the window.
	Exhausted
	// Unavailable: the consumer's quota is not known, because the profiles have not been read or
	// its profile is missing or broken. aisa fails closed.
	Unavailable
)

// Verdict is the answer of a quota check.
type Verdict struct {
	Outcome Outcome
	// Limit and Used are the consumer's tokens per hour and those used in the window, when
	// known.
	Limit, Used int64
	// RetryAfter is when enough tokens leave the window for the consumer to be allowed again;
	// set when Exhausted.
	RetryAfter time.Duration
	// Err says why the quota is Unavailable.
	Err error
}

// Options configure a Quota.
type Options struct {
	// Timeout bounds each call to the counter, so a slow Redis does not stall the gateway;
	// 250 ms when zero.
	Timeout time.Duration
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// Quota checks and charges token quotas.
type Quota struct {
	profiles *Profiles
	counter  Counter
	metrics  *metrics.Metrics
	log      *slog.Logger
	opts     Options
}

// New returns the quota checks of the profiles, counted with counter.
func New(p *Profiles, c Counter, m *metrics.Metrics, log *slog.Logger, opts Options) *Quota {
	if opts.Timeout <= 0 {
		opts.Timeout = 250 * time.Millisecond
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Quota{profiles: p, counter: c, metrics: m, log: log, opts: opts}
}

// Check decides whether a consumer may send a request. The request is allowed while the tokens
// used in the window are fewer than the profile's limit; its own tokens are known only after
// the response, so the last request can take a consumer past the limit.
//
// When the usage cannot be read from Redis, the request is allowed: the credential has been
// checked, and a quota only limits the rate. It is counted in aisa_quota_errors_total.
func (q *Quota) Check(ctx context.Context, c consumers.Consumer) Verdict {
	if c.QuotaProfile == "" {
		return Verdict{Outcome: Allow}
	}
	prof, err := q.profiles.Get(c.QuotaProfile)
	if err != nil {
		return Verdict{Outcome: Unavailable, Err: err}
	}
	now := q.opts.Now()
	ctx, cancel := context.WithTimeout(ctx, q.opts.Timeout)
	defer cancel()
	usage, err := q.counter.Usage(ctx, c.Name, now)
	if err != nil {
		q.metrics.QuotaErrors.WithLabelValues(metrics.QuotaOpCheck).Inc()
		q.log.Warn("cannot read quota usage; allowing the request", "consumer", c.Name, "error", err)
		return Verdict{Outcome: Allow, Limit: prof.TokensPerHour}
	}
	var used int64
	for _, n := range usage {
		used += n
	}
	v := Verdict{Outcome: Allow, Limit: prof.TokensPerHour, Used: used}
	if used >= prof.TokensPerHour {
		v.Outcome = Exhausted
		v.RetryAfter = retryAfter(usage, prof.TokensPerHour, now)
	}
	return v
}

// retryAfter is how long until the oldest buckets leave the window and take the usage below
// limit. A limit of 0 is never reached, so it is the whole window.
func retryAfter(usage []int64, limit int64, now time.Time) time.Duration {
	var used int64
	for _, n := range usage {
		used += n
	}
	first := minute(now) - int64(len(usage)) + 1
	for i, n := range usage {
		used -= n
		if used < limit {
			// Bucket i counts until the minute that begins a window after its own.
			leaves := time.Unix((first+int64(i)+buckets)*int64(bucket/time.Second), 0)
			return max(leaves.Sub(now), time.Second)
		}
	}
	return window
}

// Charge counts tokens that a consumer used at a time. Tokens older than the window no longer
// count and are not added; a time in the future is taken as now. An error is logged and counted
// in aisa_quota_errors_total, and returned.
func (q *Quota) Charge(ctx context.Context, consumer string, tokens int64, at time.Time) error {
	if consumer == "" || consumer == metrics.ConsumerUnknown || tokens <= 0 {
		return nil
	}
	now := q.opts.Now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	if now.Sub(at) >= window {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, q.opts.Timeout)
	defer cancel()
	if err := q.counter.Add(ctx, consumer, tokens, at); err != nil {
		q.metrics.QuotaErrors.WithLabelValues(metrics.QuotaOpCharge).Inc()
		q.log.Warn("cannot charge tokens to the quota", "consumer", consumer, "tokens", tokens, "error", err)
		return err
	}
	return nil
}
