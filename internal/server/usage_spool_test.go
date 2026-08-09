package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// fakeSpool captures what the flusher would have written to the bucket.
type fakeSpool struct {
	mu       sync.Mutex
	objects  map[string][]byte
	puts     int
	failNext error
}

func newFakeSpool() *fakeSpool {
	return &fakeSpool{objects: map[string][]byte{}}
}

func (f *fakeSpool) put(_ context.Context, object string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return err
	}
	f.objects[object] = append([]byte(nil), data...)
	return nil
}

func (f *fakeSpool) lines(t *testing.T) []registry.UsageInterval {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.UsageInterval
	for _, body := range f.objects {
		for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
			if line == "" {
				continue
			}
			var iv registry.UsageInterval
			if err := json.Unmarshal([]byte(line), &iv); err != nil {
				t.Fatalf("spool object is not valid JSONL: %v (line %q)", err, line)
			}
			out = append(out, iv)
		}
	}
	return out
}

func testSpoolServer(t *testing.T) (*Server, *fakeSpool) {
	t.Helper()
	s := testMeteringServer(t)
	spool := newFakeSpool()
	s.usagePut = spool.put
	s.usageBucketName = "test-bucket"
	return s, spool
}

// closeInterval opens and closes one interval, the shape the spool consumes.
func closeInterval(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := context.Background()
	s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm-" + id})
	if _, _, err := s.reg.CloseUsageInterval(ctx, id, registry.EndDestroy, 1_000_000); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func TestFlushUsageWritesClosedIntervalsAsJSONL(t *testing.T) {
	s, spool := testSpoolServer(t)
	ctx := context.Background()

	closeInterval(t, s, "sb1")
	closeInterval(t, s, "sb2")
	// An OPEN interval is not a fact yet and must not be spooled.
	s.meterStart(ctx, registry.Sandbox{ID: "sb3", VMID: "vm-3"})

	n, err := s.flushUsage(ctx)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != 2 {
		t.Fatalf("flushed %d intervals, want 2", n)
	}

	got := spool.lines(t)
	if len(got) != 2 {
		t.Fatalf("spool holds %d intervals, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, iv := range got {
		seen[iv.SandboxID] = true
		if iv.EndedAt == nil {
			t.Fatalf("spooled an open interval: %+v", iv)
		}
		if iv.Vcpus == 0 || iv.MemMIB == 0 {
			t.Fatalf("spooled interval lost its effective resources: %+v", iv)
		}
	}
	if seen["sb3"] {
		t.Fatal("an open interval reached the spool")
	}

	// Nothing pending afterwards, and a second flush is a no-op.
	if pending, _ := s.reg.CountUnflushedUsageIntervals(ctx); pending != 0 {
		t.Fatalf("%d intervals still unflushed", pending)
	}
	if again, err := s.flushUsage(ctx); err != nil || again != 0 {
		t.Fatalf("second flush wrote %d intervals (err %v), want 0", again, err)
	}
}

// The stamp happens after the write, so a failure between them must leave the
// batch pending rather than marking it durable when it isn't.
func TestFlushUsageKeepsBatchPendingWhenWriteFails(t *testing.T) {
	s, spool := testSpoolServer(t)
	ctx := context.Background()

	closeInterval(t, s, "sb1")
	spool.failNext = errors.New("bucket unavailable")

	if _, err := s.flushUsage(ctx); err == nil {
		t.Fatal("flush should surface the write failure")
	}
	if pending, _ := s.reg.CountUnflushedUsageIntervals(ctx); pending != 1 {
		t.Fatalf("%d intervals pending after a failed write, want 1 (billing data must not be marked durable)", pending)
	}

	// The retry succeeds and the interval becomes durable.
	if n, err := s.flushUsage(ctx); err != nil || n != 1 {
		t.Fatalf("retry flushed %d (err %v), want 1", n, err)
	}
	if pending, _ := s.reg.CountUnflushedUsageIntervals(ctx); pending != 0 {
		t.Fatalf("%d intervals still pending after a successful retry", pending)
	}
}

// A crash between the write and the stamp re-spools the batch. The object name
// is content-derived, so the retry rewrites the SAME object instead of
// accumulating near-duplicates, and consumers dedup on interval id.
func TestFlushUsageRetryIsIdempotentOnObjectName(t *testing.T) {
	s, spool := testSpoolServer(t)
	ctx := context.Background()

	closeInterval(t, s, "sb1")
	batch, err := s.reg.UnflushedUsageIntervals(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	payload, err := encodeUsageJSONL(batch)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	first := usageSpoolObject(s.hostID(), batch, payload)

	// Simulate the crash: write landed, stamp did not.
	if err := spool.put(ctx, first, payload); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.flushUsage(ctx); err != nil {
		t.Fatalf("flush after partial write: %v", err)
	}

	spool.mu.Lock()
	objects := len(spool.objects)
	spool.mu.Unlock()
	if objects != 1 {
		t.Fatalf("re-spool created %d objects, want 1 (name must be content-derived)", objects)
	}
}

// A backlog must clear on one tick rather than trickling one batch per
// interval, or an outage's worth of billing stays undurable for hours.
func TestFlushUsageDrainsMultipleBatches(t *testing.T) {
	s, spool := testSpoolServer(t)
	ctx := context.Background()

	for i := 0; i < usageFlushBatch+7; i++ {
		id := "sb" + strconv.Itoa(i)
		s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm-" + id})
		if _, _, err := s.reg.CloseUsageInterval(ctx, id, registry.EndDestroy, 1000); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}

	n, err := s.flushUsage(ctx)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != usageFlushBatch+7 {
		t.Fatalf("flushed %d, want %d", n, usageFlushBatch+7)
	}
	if spool.puts < 2 {
		t.Fatalf("expected more than one object for a >batch backlog, got %d", spool.puts)
	}
	if pending, _ := s.reg.CountUnflushedUsageIntervals(ctx); pending != 0 {
		t.Fatalf("%d intervals still pending after a full drain", pending)
	}
}

// With no bucket configured the ledger IS the record of truth, so pruning must
// never delete it. This is the self-hosted path, and the cutoff here is
// deliberately far in the future: even a row that qualifies on every other
// criterion must survive.
func TestPruneUsageIsDisabledWithoutABucket(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()
	s.usagePut = nil

	closeInterval(t, s, "sb1")

	s.pruneUsageAt(ctx, time.Now().UTC().Add(time.Hour))

	if got, _ := s.reg.UsageForSandbox(ctx, "sb1"); len(got) != 1 {
		t.Fatal("pruned the only copy of a billing record with no bucket configured")
	}
}

// Pruning is bounded by durability AND retention: only a spooled interval past
// the cutoff goes.
func TestPruneUsageDropsDurableIntervalsPastRetention(t *testing.T) {
	s, _ := testSpoolServer(t)
	ctx := context.Background()

	closeInterval(t, s, "durable")
	if _, err := s.flushUsage(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Closed after the flush, so it is past the cutoff but NOT yet durable.
	closeInterval(t, s, "pending")

	s.pruneUsageAt(ctx, time.Now().UTC().Add(time.Hour))

	if got, _ := s.reg.UsageForSandbox(ctx, "durable"); len(got) != 0 {
		t.Fatal("durable interval past retention was not pruned")
	}
	if got, _ := s.reg.UsageForSandbox(ctx, "pending"); len(got) != 1 {
		t.Fatal("pruned an interval that never reached the bucket")
	}
}

// The final flush on a graceful stop is the one that matters most: shutdownAll
// freezes every sandbox, closing their intervals, and a deleted instance never
// gets another chance.
func TestDrainUsageFlushesOnShutdown(t *testing.T) {
	s, spool := testSpoolServer(t)
	ctx := context.Background()

	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1"})
	s.shuttingDown.Store(true)
	s.meterStop(ctx, "sb1", registry.EndHibernate)

	s.drainUsage()

	got := spool.lines(t)
	if len(got) != 1 {
		t.Fatalf("drain spooled %d intervals, want 1", len(got))
	}
	if got[0].EndReason != registry.EndShutdown {
		t.Fatalf("end_reason = %q, want %q", got[0].EndReason, registry.EndShutdown)
	}
}

func TestUsageSpoolObjectNaming(t *testing.T) {
	ended := time.Date(2026, 8, 3, 14, 30, 0, 0, time.UTC)
	batch := []registry.UsageInterval{{ID: "h:sb:1", EndedAt: &ended}}
	payload := []byte("{}\n")

	got := usageSpoolObject("worker-7", batch, payload)
	if !strings.HasPrefix(got, "usage/worker-7/2026-08-03/") {
		t.Fatalf("object %q should be partitioned by host and end date", got)
	}
	if !strings.HasSuffix(got, ".jsonl") {
		t.Fatalf("object %q should be .jsonl", got)
	}
	// Pure function of its inputs — that is what makes a retry idempotent.
	if again := usageSpoolObject("worker-7", batch, payload); again != got {
		t.Fatalf("name is not deterministic: %q vs %q", got, again)
	}
	// Different content must not collide onto the same object.
	if other := usageSpoolObject("worker-7", batch, []byte("{\"a\":1}\n")); other == got {
		t.Fatal("different payloads produced the same object name: a flush could overwrite billing evidence")
	}
	// A host id with a path separator must not reshape the layout.
	if dodgy := usageSpoolObject("a/../b", batch, payload); strings.Contains(dodgy, "/../") {
		t.Fatalf("host id was not sanitized into one path segment: %q", dodgy)
	}
}

// Bucket resolution decides whether a deployment gets billing durability at
// all, so the fallback is pinned: an existing fleet with a snapshot bucket must
// start spooling usage with no infrastructure change, and a deployment with
// neither bucket must stay host-local (which also disables pruning).
func TestUsageBucketResolution(t *testing.T) {
	for _, tc := range []struct {
		name            string
		usage, snapshot string
		wantBucket      string
		wantDurable     bool
	}{
		{name: "neither: host-local", wantDurable: false},
		{name: "snapshot only: inherits it", snapshot: "snaps", wantBucket: "snaps", wantDurable: true},
		{name: "dedicated usage bucket", usage: "bills", snapshot: "snaps", wantBucket: "bills", wantDurable: true},
		{name: "usage only", usage: "bills", wantBucket: "bills", wantDurable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
				TapPrefix: "fc", TapMax: 1, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
				PortMin: 5200, PortMax: 5200,
			})
			if err != nil {
				t.Fatalf("open registry: %v", err)
			}
			t.Cleanup(func() { reg.Close() })

			s := New(Config{UsageBucket: tc.usage, SnapshotBucket: tc.snapshot}, reg)
			if durable := s.usagePut != nil; durable != tc.wantDurable {
				t.Fatalf("durable = %v, want %v", durable, tc.wantDurable)
			}
			if s.usageBucketName != tc.wantBucket {
				t.Fatalf("usage bucket = %q, want %q", s.usageBucketName, tc.wantBucket)
			}
		})
	}
}
