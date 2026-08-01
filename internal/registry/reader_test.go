package registry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reader handle exists so data-plane traffic stops queueing behind creates.
// These tests pin the three properties that make it safe: it sees committed
// writes immediately (no staleness was introduced), it cannot write (a
// misrouted write fails loudly instead of reintroducing multi-writer lock
// upgrades), and it does not serialize against an open write transaction (the
// whole point).

func TestReaderSeesCommittedWritesImmediately(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	if _, err := r.Create(ctx, "sb1", "one", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Get/Ports/Stats all run on the read-only handle. Read-your-writes must
	// hold: WAL makes a commit visible to every read that starts after it.
	if got, err := r.Get(ctx, "sb1"); err != nil || got.Name != "one" {
		t.Fatalf("Get after create = %+v, %v", got, err)
	}
	if _, err := r.AddPort(ctx, "sb1", 3000); err != nil {
		t.Fatalf("add port: %v", err)
	}
	ports, err := r.Ports(ctx, "sb1")
	if err != nil || len(ports) != 1 {
		t.Fatalf("Ports after AddPort = %+v, %v", ports, err)
	}
	if err := r.SetName(ctx, "sb1", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, _ := r.Get(ctx, "sb1"); got.Name != "renamed" {
		t.Fatalf("Get after rename = %q, want renamed", got.Name)
	}
	if st, err := r.Stats(ctx); err != nil || st.Running != 1 || st.PortUsed != 1 {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
}

func TestReaderHandleRejectsWrites(t *testing.T) {
	r := testRegistry(t)
	// Routing a write to the reader by mistake must fail at the driver, not
	// silently open a second writer: two writers on one SQLite file deadlock on
	// the lock upgrade inside Create's read-then-insert transaction, which is
	// exactly what SetMaxOpenConns(1) on the writer prevents.
	_, err := r.rdb.Exec(`INSERT INTO sandboxes (id, pid, vm_id, socket_path, tap_device, guest_ip, rootfs_path, status, created_at)
		VALUES ('x', 0, '', '', '', '', '', 'running', 0)`)
	if err == nil {
		t.Fatal("write on the read-only handle succeeded; the reader is not read-only")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("write on the read-only handle failed with %v, want a readonly error", err)
	}
}

func TestReadsDoNotQueueBehindAnOpenWriteTransaction(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Hold the writer's single connection in a write transaction, the way
	// Create/Wake do across their pool scans plus insert.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sandboxes SET name='mid-tx' WHERE id='sb1'`); err != nil {
		tx.Rollback()
		t.Fatalf("write inside tx: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// The data plane's reads: resolve the sandbox, sample capacity.
		if _, err := r.Get(rctx, "sb1"); err != nil {
			done <- err
			return
		}
		if _, err := r.Ports(rctx, "sb1"); err != nil {
			done <- err
			return
		}
		_, _, err := r.RoutedCapacity(rctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reads blocked/failed while a write transaction was open: %v", err)
		}
	case <-time.After(3 * time.Second):
		tx.Rollback()
		t.Fatal("reads did not complete while a write transaction was open — the data plane is still serialized behind creates")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got, _ := r.Get(ctx, "sb1"); got.Name != "mid-tx" {
		t.Fatalf("post-commit read = %q, want mid-tx", got.Name)
	}
}

func TestAllocationStaysAtomicUnderConcurrentReaderTraffic(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     16,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.25",
		PortMin:    5200,
		PortMax:    5299,
	})
	ctx := context.Background()

	// Creates allocate tap/IP inside one transaction on the writer; the reader
	// pool is hammered simultaneously with the exact queries the data plane and
	// the heartbeat issue. Allocation must remain exclusive (no two sandboxes
	// share a tap or IP) and no read may fail with SQLITE_BUSY.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := r.Get(ctx, "sb-3"); err != nil && err.Error() != "sql: no rows in result set" {
					t.Errorf("concurrent Get: %v", err)
					return
				}
				if _, _, err := r.RoutedCapacity(ctx); err != nil {
					t.Errorf("concurrent RoutedCapacity: %v", err)
					return
				}
				if _, err := r.PublicRoutes(ctx); err != nil {
					t.Errorf("concurrent PublicRoutes: %v", err)
					return
				}
				if _, err := r.Stats(ctx); err != nil {
					t.Errorf("concurrent Stats: %v", err)
					return
				}
			}
		}()
	}

	var creates sync.WaitGroup
	var mu sync.Mutex
	taps, ips := map[string]string{}, map[string]string{}
	for i := 0; i < 16; i++ {
		creates.Add(1)
		go func(i int) {
			defer creates.Done()
			id := "sb-" + string(rune('a'+i))
			sb, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0)
			if err != nil {
				t.Errorf("create %s: %v", id, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if prev, dup := taps[sb.TapDevice]; dup {
				t.Errorf("tap %s allocated to both %s and %s", sb.TapDevice, prev, id)
			}
			if prev, dup := ips[sb.GuestIP]; dup {
				t.Errorf("IP %s allocated to both %s and %s", sb.GuestIP, prev, id)
			}
			taps[sb.TapDevice], ips[sb.GuestIP] = id, id
		}(i)
	}
	creates.Wait()
	close(stop)
	readers.Wait()

	if len(taps) != 16 || len(ips) != 16 {
		t.Fatalf("allocated %d taps / %d IPs, want 16 each", len(taps), len(ips))
	}
}

func TestPublicRoutesReturnsEveryRouteInOneQuery(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     4,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.13",
		PortMin:    5200,
		PortMax:    5203,
	})
	ctx := context.Background()

	want := map[int]PublicRoute{}
	for i, id := range []string{"a", "b", "c", "d"} {
		if _, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		guestPort := 3000 + i
		if _, err := r.AddURLPort(ctx, id, guestPort); err != nil {
			t.Fatalf("expose %s: %v", id, err)
		}
		public := 20000 + i
		if err := r.SetPublicPort(ctx, id, guestPort, public); err != nil {
			t.Fatalf("set public port %s: %v", id, err)
		}
		want[public] = PublicRoute{SandboxID: id, GuestPort: guestPort, PublicPort: public}
	}
	// A mapping with no public port must not appear: raw ingress routes only.
	if _, err := r.AddURLPort(ctx, "a", 4000); err != nil {
		t.Fatalf("expose extra: %v", err)
	}

	before, _ := r.PortReadCounts()
	got, err := r.PublicRoutes(ctx)
	if err != nil {
		t.Fatalf("PublicRoutes: %v", err)
	}
	after, portsQueries := r.PortReadCounts()
	if after-before != 1 {
		t.Fatalf("PublicRoutes issued %d queries, want 1", after-before)
	}
	if portsQueries != 0 {
		t.Fatalf("PublicRoutes fell back to %d per-sandbox Ports queries, want 0", portsQueries)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routes, want %d: %+v", len(got), len(want), got)
	}
	for _, pr := range got {
		if w, ok := want[pr.PublicPort]; !ok || w != pr {
			t.Fatalf("unexpected route %+v", pr)
		}
	}
}
