package quotas

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// The window is an hour of one-minute buckets: a consumer's usage is the sum of the bucket of
// the current minute and the 59 before it. It slides by the minute, so a request is counted for
// between 59 and 60 minutes.
const (
	bucket  = time.Minute
	buckets = 60
	window  = buckets * bucket
)

// Window counts tokens per consumer in Redis.
type Window struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewWindow returns a window whose keys begin with prefix, such as "aisa:".
func NewWindow(rdb redis.UniversalClient, prefix string) *Window {
	return &Window{rdb: rdb, prefix: prefix}
}

// minute is the number of a bucket: minutes since the epoch.
func minute(t time.Time) int64 { return t.Unix() / int64(bucket/time.Second) }

// key is the key of a consumer's bucket. The consumer is the hash tag, so a consumer's keys
// are in one slot of a Redis cluster and one MGET reads them.
func (w *Window) key(consumer string, m int64) string {
	return w.prefix + "quota:{" + consumer + "}:" + strconv.FormatInt(m, 10)
}

// Add counts tokens that a consumer used at a time. A bucket expires when it has left the
// window.
func (w *Window) Add(ctx context.Context, consumer string, tokens int64, at time.Time) error {
	m := minute(at)
	key := w.key(consumer, m)
	expire := time.Unix((m+buckets+1)*int64(bucket/time.Second), 0)
	// MULTI/EXEC: a bucket never exists without its expiry.
	_, err := w.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.IncrBy(ctx, key, tokens)
		p.ExpireAt(ctx, key, expire)
		return nil
	})
	if err != nil {
		return fmt.Errorf("redis: add tokens: %w", err)
	}
	return nil
}

// Usage returns a consumer's buckets in the window that ends at now, the oldest first.
func (w *Window) Usage(ctx context.Context, consumer string, now time.Time) ([]int64, error) {
	cur := minute(now)
	keys := make([]string, buckets)
	for i := range keys {
		keys[i] = w.key(consumer, cur-buckets+1+int64(i))
	}
	vals, err := w.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: read usage: %w", err)
	}
	out := make([]int64, buckets)
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue // nil: no tokens in that minute
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: usage bucket %s: %w", keys[i], err)
		}
		out[i] = n
	}
	return out, nil
}
