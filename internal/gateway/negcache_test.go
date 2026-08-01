package gateway

import (
	"fmt"
	"testing"
	"time"
)

// TestNegCacheBounded: a flood of DISTINCT ids — the exact shape of an unknown-
// hostname scan through the public edge — can never grow the cache past its cap,
// and the eviction bookkeeping (order) is bounded too. This is audit X12.
func TestNegCacheBounded(t *testing.T) {
	c := newNegCache(time.Hour, 64) // long TTL: nothing expires, so only the cap can bound it
	for i := 0; i < 10_000; i++ {
		c.add(fmt.Sprintf("id-%d", i))
		if n := c.len(); n > 64 {
			t.Fatalf("after %d inserts len=%d, want <= 64", i+1, n)
		}
	}
	if len(c.order) > 2*64 {
		t.Fatalf("eviction queue grew to %d, want <= 128", len(c.order))
	}
	// The most recent inserts survive; the oldest were evicted.
	if !c.has("id-9999") {
		t.Fatal("newest entry evicted")
	}
	if c.has("id-0") {
		t.Fatal("oldest entry still cached past the cap")
	}
}

// TestNegCacheExpiryAndDrop: an entry stops answering once its TTL passes, and
// drop forgets it immediately (a landed route must not sit behind a stale
// negative).
func TestNegCacheExpiryAndDrop(t *testing.T) {
	c := newNegCache(20*time.Millisecond, 8)
	c.add("gone")
	if !c.has("gone") {
		t.Fatal("fresh entry not cached")
	}
	time.Sleep(30 * time.Millisecond)
	if c.has("gone") {
		t.Fatal("expired entry still cached")
	}
	if n := c.len(); n != 0 {
		t.Fatalf("expired entry not swept on read: len=%d", n)
	}

	c.add("later")
	c.drop("later")
	if c.has("later") {
		t.Fatal("dropped entry still cached")
	}
}

// TestNegCacheRepeatedIDDoesNotGrowQueue: re-adding the same id refreshes its
// expiry without queueing a second eviction entry.
func TestNegCacheRepeatedIDDoesNotGrowQueue(t *testing.T) {
	c := newNegCache(time.Hour, 8)
	for i := 0; i < 100; i++ {
		c.add("same")
	}
	if c.len() != 1 || len(c.order) != 1 {
		t.Fatalf("len=%d order=%d, want 1 and 1", c.len(), len(c.order))
	}
}
