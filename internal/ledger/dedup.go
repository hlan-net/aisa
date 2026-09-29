// Package ledger ingests usage events from gateways, deduplicates them by request id,
// and records token and latency metrics.
package ledger

import (
	"container/list"
	"sync"
	"time"
)

type dedupEntry struct {
	key       string
	expiresAt time.Time
}

// Dedup tracks seen request IDs to deduplicate retried usage events.
type Dedup struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*list.Element
	order    *list.List
	now      func() time.Time
}

// NewDedup creates a Dedup cache with the given capacity and TTL.
func NewDedup(capacity int, ttl time.Duration) *Dedup {
	if capacity <= 0 {
		capacity = 100_000
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Dedup{
		capacity: capacity,
		ttl:      ttl,
		items:    make(map[string]*list.Element),
		order:    list.New(),
		now:      time.Now,
	}
}

// SeenOrAdd reports whether key has already been seen within the TTL window.
// If it has not been seen or has expired, it records key with an expiration of now + TTL and reports false.
// If key is empty, it reports false without recording it.
func (d *Dedup) SeenOrAdd(key string) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()

	if elem, ok := d.items[key]; ok {
		entry := elem.Value.(*dedupEntry)
		if now.Before(entry.expiresAt) {
			d.order.MoveToFront(elem)
			return true
		}
		// Expired: renew TTL and move to front
		entry.expiresAt = now.Add(d.ttl)
		d.order.MoveToFront(elem)
		return false
	}

	// Evict expired entries from the back
	for d.order.Len() > 0 {
		back := d.order.Back()
		entry := back.Value.(*dedupEntry)
		if now.After(entry.expiresAt) {
			d.removeElement(back)
		} else {
			break
		}
	}

	// Evict the least recently used element if at capacity
	for d.order.Len() >= d.capacity {
		back := d.order.Back()
		if back == nil {
			break
		}
		d.removeElement(back)
	}

	entry := &dedupEntry{
		key:       key,
		expiresAt: now.Add(d.ttl),
	}
	elem := d.order.PushFront(entry)
	d.items[key] = elem
	return false
}

func (d *Dedup) removeElement(elem *list.Element) {
	d.order.Remove(elem)
	entry := elem.Value.(*dedupEntry)
	delete(d.items, entry.key)
}
