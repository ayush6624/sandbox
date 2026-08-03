package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

const (
	// usageFlushInterval bounds how much billing evidence a host can be holding
	// only locally. A worker's SQLite lives on its data disk, which a MIG
	// scale-in that DELETES an instance takes with it, so this is the real
	// exposure window for lost revenue.
	usageFlushInterval = 5 * time.Minute
	// usageFlushBatch caps one object's size. Intervals are small (~300 bytes),
	// so this keeps a flush well under a MiB while still draining a backlog
	// quickly across successive ticks.
	usageFlushBatch = 1000
	// usageRetention is how long CLOSED, ALREADY-SPOOLED intervals stay in the
	// local ledger to answer recent-usage reads without a bucket round trip.
	// The bucket is the record of truth past this point.
	usageRetention = 7 * 24 * time.Hour
)

// Durable spool for billable usage.
//
// One flush writes one immutable object. Nothing is ever rewritten or deleted
// here, because the bucket is the billing record: local rows are a cache in
// front of it, and the flush is the only thing standing between a scaled-in
// worker and revenue that can never be reconstructed.
//
// The write is at-least-once by design — `flushed_at` is stamped AFTER the
// object lands, so a crash in between re-spools the batch. Consumers dedup on
// the deterministic interval id, which is why that id is
// "<host>:<sandbox>:<seq>" and not a UUID.

// usageSpooler flushes closed intervals to the bucket and prunes what is
// already durable. Started only when a bucket is configured.
func (s *Server) usageSpooler(ctx context.Context) {
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := s.flushUsage(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "usage: flush to gs://%s: %v\n", s.usageBucketName, err)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "usage: spooled %d interval(s) to gs://%s\n", n, s.usageBucketName)
			}
			s.pruneUsage(ctx)
		}
	}
}

// flushUsage writes every closed-but-unspooled interval to the bucket and marks
// it durable. Returns how many intervals landed.
//
// Drains in batches until nothing is pending, so a backlog (an outage, a long
// bucket failure) clears on the first successful tick instead of trickling at
// one batch per five minutes.
func (s *Server) flushUsage(ctx context.Context) (int, error) {
	if s.usagePut == nil {
		return 0, nil
	}
	total := 0
	for {
		batch, err := s.reg.UnflushedUsageIntervals(ctx, usageFlushBatch)
		if err != nil {
			return total, fmt.Errorf("list unflushed: %w", err)
		}
		if len(batch) == 0 {
			return total, nil
		}
		payload, err := encodeUsageJSONL(batch)
		if err != nil {
			return total, fmt.Errorf("encode batch: %w", err)
		}
		object := usageSpoolObject(s.hostID(), batch, payload)
		if err := s.usagePut(ctx, object, payload); err != nil {
			return total, fmt.Errorf("put %s: %w", object, err)
		}
		// Only now is the batch durable. A crash before this stamp re-spools it
		// — the object name is content-derived, so the retry overwrites the
		// identical object rather than accumulating garbage, and any consumer
		// that already read the first copy dedups on interval id.
		ids := make([]string, len(batch))
		for i, iv := range batch {
			ids[i] = iv.ID
		}
		if err := s.reg.MarkUsageFlushed(ctx, ids); err != nil {
			return total, fmt.Errorf("mark flushed: %w", err)
		}
		total += len(batch)
		if len(batch) < usageFlushBatch {
			return total, nil
		}
	}
}

// pruneUsage drops local rows that are closed AND durable AND past retention.
//
// Gated on a configured bucket: with nowhere to spool to, `flushed_at` is never
// set and this deletes nothing — which is the point. A self-hosted deployment
// without object storage keeps its ledger locally forever rather than quietly
// discarding the only copy of its billing data.
func (s *Server) pruneUsage(ctx context.Context) {
	s.pruneUsageAt(ctx, time.Now().UTC().Add(-usageRetention))
}

// pruneUsageAt is pruneUsage with an explicit cutoff, so the gating and the
// retention predicate are testable without rewriting timestamps underneath the
// registry's single-writer connection.
func (s *Server) pruneUsageAt(ctx context.Context, cutoff time.Time) {
	if s.usagePut == nil {
		return
	}
	n, err := s.reg.PruneUsageIntervals(ctx, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: prune spooled intervals: %v\n", err)
		return
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "usage: pruned %d local interval(s) older than %s (durable in gs://%s)\n",
			n, usageRetention, s.usageBucketName)
	}
}

// drainUsage makes a graceful shutdown's freezes durable before the process
// exits. shutdownAll hibernates every sandbox, which CLOSES their intervals —
// so the most valuable flush of a worker's life is the last one, and on a MIG
// scale-in that deletes the instance it is the only one that can still happen.
func (s *Server) drainUsage() {
	if s.usagePut == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	n, err := s.flushUsage(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: final flush failed, %d interval(s) may be host-local only: %v\n", n, err)
		return
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "usage: final flush spooled %d interval(s)\n", n)
	}
}

// encodeUsageJSONL renders a batch as newline-delimited JSON: one interval per
// line, so a consumer can stream it and a partial object still parses up to its
// last complete line.
func encodeUsageJSONL(batch []registry.UsageInterval) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, iv := range batch {
		if err := enc.Encode(iv); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// usageSpoolObject names a flush.
//
// The name is derived from the batch's CONTENT, not from a counter: a process
// restart resets a counter, and a colliding name would silently overwrite a
// previous flush — destroying billing evidence, which is the one thing this
// spool exists to prevent. Content-addressing makes a retry idempotent instead.
//
// The date and leading timestamp come from the batch's last end time so the
// listing sorts by when usage happened, and so the name is a pure function of
// its input (testable, and identical on retry).
func usageSpoolObject(hostID string, batch []registry.UsageInterval, payload []byte) string {
	end := time.Unix(0, 0).UTC()
	for _, iv := range batch {
		if iv.EndedAt != nil && iv.EndedAt.After(end) {
			end = *iv.EndedAt
		}
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("usage/%s/%s/%d-%s.jsonl",
		usageObjectSegment(hostID), end.Format("2006-01-02"), end.Unix(), hex.EncodeToString(sum[:4]))
}

// usageObjectSegment keeps a host id usable as one GCS path segment. Host ids
// are hostnames or configured labels, but a stray '/' would silently reshape the
// object layout, so anything outside a conservative set is replaced.
func usageObjectSegment(hostID string) string {
	if hostID == "" {
		return "unknown"
	}
	out := make([]rune, 0, len(hostID))
	for _, r := range hostID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
