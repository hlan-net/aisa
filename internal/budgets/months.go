package budgets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Months counts a consumer's tokens per calendar month in Redis.
type Months struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewMonths returns a counter whose keys begin with prefix, such as "aisa:".
func NewMonths(rdb redis.UniversalClient, prefix string) *Months {
	return &Months{rdb: rdb, prefix: prefix}
}

// keep is how long a month's counter stays after the month: long enough to be read for a
// report, and gone before the key could be confused with a year later.
const keep = 35 * 24 * time.Hour

// key is the key of a consumer's month. The consumer is the hash tag, so a consumer's keys
// are in one slot of a Redis cluster, as the quota window's are.
func (m *Months) key(consumer, period string) string {
	return m.prefix + "budget:{" + consumer + "}:" + period
}

// Add counts tokens that a consumer used in a period and returns the period's total. floor
// is the total aisa last persisted for the period: a counter that is missing, because Redis
// lost its data, starts from it rather than from zero.
func (m *Months) Add(ctx context.Context, consumer, period string, tokens, floor int64, end time.Time) (int64, error) {
	key := m.key(consumer, period)
	var incr *redis.IntCmd
	// MULTI/EXEC: the counter never exists without its floor and its expiry.
	_, err := m.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if floor > 0 {
			p.SetNX(ctx, key, floor, 0)
		}
		incr = p.IncrBy(ctx, key, tokens)
		p.ExpireAt(ctx, key, end.Add(keep))
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("redis: add budget tokens: %w", err)
	}
	return incr.Val(), nil
}

// Total returns the tokens a consumer has used in a period: 0 when Redis has no counter.
func (m *Months) Total(ctx context.Context, consumer, period string) (int64, error) {
	n, err := m.rdb.Get(ctx, m.key(consumer, period)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis: read budget usage: %w", err)
	}
	return n, nil
}
