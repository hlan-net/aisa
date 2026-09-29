package ledger

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDedupBasic(t *testing.T) {
	d := NewDedup(10, 5*time.Minute)

	if d.SeenOrAdd("req-1") {
		t.Error("first SeenOrAdd(req-1) should return false")
	}
	if !d.SeenOrAdd("req-1") {
		t.Error("second SeenOrAdd(req-1) should return true")
	}
	if d.SeenOrAdd("req-2") {
		t.Error("first SeenOrAdd(req-2) should return false")
	}
	if !d.SeenOrAdd("req-2") {
		t.Error("second SeenOrAdd(req-2) should return true")
	}
}

func TestDedupEmptyKey(t *testing.T) {
	d := NewDedup(10, 5*time.Minute)

	if d.SeenOrAdd("") {
		t.Error("SeenOrAdd with empty key should return false")
	}
	if d.SeenOrAdd("") {
		t.Error("SeenOrAdd with empty key again should return false")
	}
}

func TestDedupExpiry(t *testing.T) {
	d := NewDedup(10, 5*time.Minute)
	currentTime := time.Now()
	d.now = func() time.Time { return currentTime }

	if d.SeenOrAdd("req-1") {
		t.Error("first SeenOrAdd should return false")
	}
	if !d.SeenOrAdd("req-1") {
		t.Error("immediate second SeenOrAdd should return true")
	}

	// Advance time past TTL
	currentTime = currentTime.Add(5*time.Minute + time.Second)

	if d.SeenOrAdd("req-1") {
		t.Error("SeenOrAdd after TTL should return false")
	}
	if !d.SeenOrAdd("req-1") {
		t.Error("SeenOrAdd right after renewal should return true")
	}
}

func TestDedupCapacity(t *testing.T) {
	d := NewDedup(3, 10*time.Minute)

	// Add 3 items: req-1, req-2, req-3
	_ = d.SeenOrAdd("req-1")
	_ = d.SeenOrAdd("req-2")
	_ = d.SeenOrAdd("req-3")

	// Touch req-1 so req-2 becomes the least recently used
	if !d.SeenOrAdd("req-1") {
		t.Error("req-1 should be duplicate")
	}

	// Add req-4 -> capacity is 3, so req-2 should be evicted
	if d.SeenOrAdd("req-4") {
		t.Error("req-4 should not be duplicate")
	}

	// req-2 should have been evicted
	if d.SeenOrAdd("req-2") {
		t.Error("req-2 should have been evicted and return false")
	}

	// Now req-3 was LRU and should have been evicted by adding req-2
	if d.SeenOrAdd("req-3") {
		t.Error("req-3 should have been evicted and return false")
	}
}

func TestDedupConcurrency(t *testing.T) {
	d := NewDedup(100, time.Minute)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("req-%d", id%10)
			_ = d.SeenOrAdd(key)
		}(i)
	}
	wg.Wait()
}
