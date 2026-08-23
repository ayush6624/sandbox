package gateway

// Scheduler-only measurements: how fast the gateway DECIDES where a sandbox
// goes, never how fast one boots. Two numbers, deliberately kept apart:
//
//   - reserveHost/release          — the placement decision itself (bin-pack
//                                    scan + reservation under g.mu)
//   - handleCreate -> fake worker  — decision plus body forward, with a worker
//                                    that answers 201 instantly
//
// A real create is ~10 ms (ready pool) to ~700 ms (clone), so anything here
// that isn't microseconds is the scheduler being the bottleneck, not the VM.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
)

// benchFleet builds n live hosts, each half full so bin-packing has to
// discriminate rather than short-circuit on the first candidate. addr is unset:
// nothing dials these hosts in reserve-only benchmarks.
func benchFleet(n, slots int) *Gateway {
	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	for i := 0; i < n; i++ {
		used := (i % (slots / 2)) // varied fullness => real comparisons
		g.hosts[fmt.Sprintf("h%04d", i)] = &host{
			// Distinct addrs: handleRegister collapses two ids sharing one
			// address into a single host, which would corrupt a churn run.
			id: fmt.Sprintf("h%04d", i), addr: hostAddr(i), token: "t",
			slotsTotal: slots, slotsUsed: used, slotsFree: slots - used,
			lastSeen: now,
		}
	}
	return g
}

func hostAddr(i int) string { return fmt.Sprintf("10.0.%d.%d:8080", i/256, i%256) }

func BenchmarkReserveRelease(b *testing.B) {
	for _, n := range []int{1, 8, 64, 512, 4096} {
		b.Run(fmt.Sprintf("hosts=%d", n), func(b *testing.B) {
			g := benchFleet(n, 48)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h := g.reserveHost(nil)
				if h == nil {
					b.Fatal("fleet exhausted")
				}
				g.releaseReservation(h, false)
			}
		})
	}
}

// BenchmarkReserveReleaseParallel is the contention number: every placement
// takes g.mu exclusively, so this is where a big fleet plus a big burst meet.
func BenchmarkReserveReleaseParallel(b *testing.B) {
	for _, n := range []int{8, 64, 512} {
		b.Run(fmt.Sprintf("hosts=%d", n), func(b *testing.B) {
			g := benchFleet(n, 48)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if h := g.reserveHost(nil); h != nil {
						g.releaseReservation(h, false)
					}
				}
			})
		})
	}
}

// BenchmarkHeartbeat measures the other half of the scheduler's lock budget:
// handleRegister rebuilds a host's routes under the SAME write lock placement
// needs, scanning the whole fleet-wide route map to do it.
func BenchmarkHeartbeat(b *testing.B) {
	for _, ids := range []int{0, 48, 1000} {
		b.Run(fmt.Sprintf("sandboxes=%d", ids), func(b *testing.B) {
			g := New("tok", 20*time.Second, 0, 0)
			body := heartbeatBody("h0", "10.0.0.1:8080", 48, ids)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rr := httptest.NewRecorder()
				g.handleRegister(rr, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
				if rr.Code != http.StatusNoContent {
					b.Fatalf("heartbeat: %d %s", rr.Code, rr.Body)
				}
			}
		})
	}
}

func heartbeatBody(id, addr string, slots, sandboxes int) []byte {
	free := slots
	ids := make([]string, sandboxes)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-sb-%06d", id, i)
	}
	b, _ := json.Marshal(cluster.Heartbeat{
		HostID: id, Addr: addr, ControlToken: "t",
		SlotsTotal: slots, SlotsFree: &free, SandboxIDs: ids,
	})
	return b
}

// BenchmarkHandleCreate is the end-to-end placement path with the VM removed:
// real JSON decode, real reserve, real forward to a worker that answers 201
// over loopback. The loopback RTT is included on purpose — it is what a create
// costs the gateway even when the worker is free.
func BenchmarkHandleCreate(b *testing.B) {
	g, _ := createFleet(b, 32, 1<<20) // effectively unbounded capacity
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rr := httptest.NewRecorder()
			g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))
			if rr.Code != http.StatusCreated {
				b.Fatalf("create: %d %s", rr.Code, rr.Body)
			}
		}
	})
}

// createFleet wires n hosts to ONE httptest worker that answers every create
// with a unique id. Returns the gateway and the worker's hit counter.
func createFleet(tb testing.TB, n, slots int) (*Gateway, *atomic.Int64) {
	tb.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"00000000-0000-4000-8000-%012d"}`, i)
	}))
	tb.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("h%04d", i)
		// Distinct addrs: handleRegister-style identity collapse aside, a shared
		// addr would make landReservation resolve every host to the first one.
		g.hosts[id] = &host{id: id, addr: addr, token: "t",
			slotsTotal: slots, slotsFree: slots, lastSeen: now}
	}
	return g, &hits
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func report(t *testing.T, label string, lat []time.Duration, wall time.Duration) {
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("%s: n=%d wall=%v rate=%.0f/s p50=%v p95=%v p99=%v max=%v",
		label, len(lat), wall.Round(time.Millisecond),
		float64(len(lat))/wall.Seconds(),
		pct(lat, 0.50), pct(lat, 0.95), pct(lat, 0.99), pct(lat, 1))
}

// TestSchedulerPlacementRate is the stress number: concurrent placements
// against a fleet with capacity to spare, reported as rate + percentiles.
func TestSchedulerPlacementRate(t *testing.T) {
	const (
		hosts   = 64
		slots   = 48
		workers = 64
		each    = 2000
	)
	g := benchFleet(hosts, slots)
	lat := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lat[w] = make([]time.Duration, 0, each)
			for i := 0; i < each; i++ {
				t0 := time.Now()
				h := g.reserveHost(nil)
				lat[w] = append(lat[w], time.Since(t0))
				if h != nil {
					g.releaseReservation(h, false)
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	report(t, fmt.Sprintf("reserve+release hosts=%d conc=%d", hosts, workers), all, wall)
}

// TestSchedulerPlacementRateUnderHeartbeatChurn re-runs the same placement
// stress while every host heartbeats at production cadence with a full sandbox
// list. Heartbeats take the same write lock and rebuild the route map, so a
// large p99 gap between this and the quiet run means the scheduler is losing to
// bookkeeping, not to placement.
func TestSchedulerPlacementRateUnderHeartbeatChurn(t *testing.T) {
	const (
		hosts     = 64
		slots     = 48
		perHostSB = 48
		workers   = 64
		each      = 500
	)
	g := benchFleet(hosts, slots)
	bodies := make([][]byte, hosts)
	for i := range bodies {
		bodies[i] = heartbeatBody(fmt.Sprintf("h%04d", i), hostAddr(i), slots, perHostSB)
	}

	stop := make(chan struct{})
	var beats atomic.Int64
	var hb sync.WaitGroup
	for i := 0; i < hosts; i++ {
		hb.Add(1)
		go func(i int) {
			defer hb.Done()
			// 5 s in production; hammering at 5 ms is a ~1000x-accelerated
			// fleet, which is the point of a stress test.
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					rr := httptest.NewRecorder()
					g.handleRegister(rr, httptest.NewRequest("POST", "/register", bytes.NewReader(bodies[i])))
					beats.Add(1)
				}
			}
		}(i)
	}

	lat := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lat[w] = make([]time.Duration, 0, each)
			for i := 0; i < each; i++ {
				t0 := time.Now()
				h := g.reserveHost(nil)
				lat[w] = append(lat[w], time.Since(t0))
				if h != nil {
					g.releaseReservation(h, false)
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)
	close(stop)
	hb.Wait()

	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	report(t, fmt.Sprintf("reserve+release under %d heartbeats", beats.Load()), all, wall)
}

// TestSchedulerEndToEndCreateRate places through the full handleCreate path
// against instant workers: the honest "creates per second the gateway can
// dispatch" figure, boot excluded.
func TestSchedulerEndToEndCreateRate(t *testing.T) {
	const (
		hosts   = 32
		workers = 64
		each    = 100
	)
	g, hits := createFleet(t, hosts, 1<<20)
	lat := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lat[w] = make([]time.Duration, 0, each)
			for i := 0; i < each; i++ {
				t0 := time.Now()
				rr := httptest.NewRecorder()
				g.handleCreate(rr, httptest.NewRequest("POST", "/sandboxes", strings.NewReader(`{}`)))
				lat[w] = append(lat[w], time.Since(t0))
				if rr.Code != http.StatusCreated {
					t.Errorf("create: %d %s", rr.Code, rr.Body)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	report(t, fmt.Sprintf("handleCreate hosts=%d conc=%d", hosts, workers), all, wall)
	if got := hits.Load(); int(got) != workers*each {
		t.Fatalf("worker saw %d creates, want %d", got, workers*each)
	}
}

// TestConcurrentPlacementNeverOverPlaces is the correctness half of the stress:
// reservations must see each other, so N concurrent placements against exactly
// C free slots must hand out exactly C — no more (over-place, which 503s on the
// worker) and no fewer (phantom capacity loss).
func TestConcurrentPlacementNeverOverPlaces(t *testing.T) {
	const hosts, slots, workers = 16, 8, 64
	capacity := hosts * slots
	g := New("tok", 20*time.Second, 0, 0)
	now := time.Now()
	for i := 0; i < hosts; i++ {
		id := fmt.Sprintf("h%02d", i)
		g.hosts[id] = &host{id: id, addr: "10.0.0.1:8080", token: "t",
			slotsTotal: slots, slotsFree: slots, lastSeen: now}
	}

	var got atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				h := g.reserveHost(nil) // held, never released
				if h == nil {
					return
				}
				got.Add(1)
			}
		}()
	}
	wg.Wait()
	if int(got.Load()) != capacity {
		t.Fatalf("placed %d of %d free slots", got.Load(), capacity)
	}
	// And the fleet must now report itself full, per-host.
	g.mu.RLock()
	defer g.mu.RUnlock()
	for id, h := range g.hosts {
		if h.free() != 0 || h.reserved != slots {
			t.Fatalf("host %s: free=%d reserved=%d, want 0/%d", id, h.free(), h.reserved, slots)
		}
	}
}

// BenchmarkHeartbeatFleetRoutes isolates the scheduler's worst asymptote:
// handleRegister's route rebuild scans the ENTIRE fleet-wide route map to find
// this host's stale entries, so one heartbeat costs O(total sandboxes) — and
// every host pays it, under the same write lock placement needs. Cost here
// should grow with fleetSandboxes even though the heartbeat itself is fixed.
func BenchmarkHeartbeatFleetRoutes(b *testing.B) {
	for _, fleetSB := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("fleetSandboxes=%d", fleetSB), func(b *testing.B) {
			g := benchFleet(64, 48)
			for i := 0; i < fleetSB; i++ {
				g.route[fmt.Sprintf("sb-%08d", i)] = fmt.Sprintf("h%04d", i%64)
			}
			body := heartbeatBody("h0000", hostAddr(0), 48, 48)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rr := httptest.NewRecorder()
				g.handleRegister(rr, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
			}
		})
	}
}
