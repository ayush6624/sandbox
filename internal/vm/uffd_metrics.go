package vm

// Per-fault latency instrumentation for the UFFD handler. Untagged so the
// histogram math is unit-testable on any host. The number that decides
// UFFD-vs-File for the GCS chunk source is p99 page-in latency on a cold wake
// (docs/uffd-roadmap.md B2c): a warm fault hits the chunk cache and is fast, a
// cold one blocks on a network fetch, and it's the tail that stalls a vCPU.

import (
	"fmt"
	"math/bits"
	"sync/atomic"
	"time"
)

// latencyBuckets covers [2^0, 2^28) µs ≈ up to 268 s per fault (far past any
// real fetch; the top bucket just absorbs pathological stalls).
const latencyBuckets = 28

// latencyHist is a lock-free log2-microsecond histogram: bucket i counts faults
// whose source-fetch latency fell in [2^i, 2^(i+1)) µs. Recorded on the fault
// hot path (two clock reads + atomic adds per fault — negligible beside a page
// copy, let alone a network fetch), summarized to the log at handler teardown.
type latencyHist struct {
	buckets [latencyBuckets]atomic.Uint64
	count   atomic.Uint64
	sumUS   atomic.Uint64
	maxUS   atomic.Uint64
}

// record files one fault's source-fetch duration.
func (h *latencyHist) record(d time.Duration) {
	us := uint64(d.Microseconds())
	h.count.Add(1)
	h.sumUS.Add(us)
	for { // maxUS = max(maxUS, us)
		cur := h.maxUS.Load()
		if us <= cur || h.maxUS.CompareAndSwap(cur, us) {
			break
		}
	}
	b := 0
	if us > 0 {
		b = bits.Len64(us) - 1 // floor(log2 us)
		if b >= latencyBuckets {
			b = latencyBuckets - 1
		}
	}
	h.buckets[b].Add(1)
}

// percentileUS returns the upper edge (µs) of the bucket holding the p-th
// percentile fault (p in [0,1]) — a coarse, bucket-granular estimate, reported
// as "p99 ≤ X µs". 0 if no faults recorded.
func (h *latencyHist) percentileUS(p float64) uint64 {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	target := uint64(p * float64(total))
	var cum uint64
	for i := 0; i < latencyBuckets; i++ {
		cum += h.buckets[i].Load()
		if cum >= target {
			return uint64(1) << uint(i+1) // upper edge of bucket i
		}
	}
	return uint64(1) << latencyBuckets
}

// summary is a one-line log string: count, mean, p50/p99 (bucket ceilings), max.
func (h *latencyHist) summary() string {
	n := h.count.Load()
	if n == 0 {
		return "no faults served"
	}
	return fmt.Sprintf("faults=%d mean=%dµs p50≤%dµs p99≤%dµs max=%dµs",
		n, h.sumUS.Load()/n, h.percentileUS(0.50), h.percentileUS(0.99), h.maxUS.Load())
}
