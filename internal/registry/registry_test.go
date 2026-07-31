package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	return testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
}

func testRegistryWithPools(t *testing.T, pools Pools) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry.db"), pools)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestHibernateFreesSlotAndWakeReclaimsIdentity(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	// The hibernated identity is free but SOFT-avoided: a new sandbox must
	// pick different resources while the pool has other entries.
	other, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create after hibernate should reuse the freed slot: %v", err)
	}
	if other.TapDevice == sb.TapDevice || other.GuestIP == sb.GuestIP {
		t.Fatalf("new sandbox squatted a hibernated identity despite free pool entries: %+v vs %+v", other, sb)
	}

	// Wake finds the old identity untouched → same-identity restore.
	woken, same, err := r.Wake(ctx, "sb1")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !same {
		t.Fatal("wake should report same identity when tap+IP are free")
	}
	if woken.TapDevice != sb.TapDevice || woken.GuestIP != sb.GuestIP {
		t.Fatalf("same-identity wake changed resources: %+v vs %+v", woken, sb)
	}
	if woken.Status != StatusRunning || woken.StoppedAt != nil {
		t.Fatalf("woken sandbox should be running with no stopped_at: %+v", woken)
	}
}

func TestWakeAllocatesFreshIdentityWhenSquatted(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	ctx := context.Background()

	sb, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	// Fill every tap slot: the last create is forced onto the hibernated
	// tap/IP — soft avoidance yields when the pool is exhausted.
	squatted := false
	for _, id := range []string{"a", "b", "c"} {
		got, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0)
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if got.TapDevice == sb.TapDevice {
			squatted = true
		}
	}
	if !squatted {
		t.Fatal("filling the pool should have forced a create onto the hibernated tap")
	}

	// No tap capacity at all → wake must fail (host is full).
	if _, _, err := r.Wake(ctx, "sb1"); err == nil {
		t.Fatal("wake with an exhausted pool should fail")
	}

	// Free one slot; wake must succeed with a fresh tap/IP.
	if err := r.Destroy(ctx, "a"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	woken, same, err := r.Wake(ctx, "sb1")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if same {
		t.Fatal("wake should report a fresh identity when the old tap/IP are taken")
	}
	if woken.Status != StatusRunning {
		t.Fatalf("woken sandbox should be running: %+v", woken)
	}
}

func TestExplicitPortStaysReservedWhileHibernated(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5201,
	})
	ctx := context.Background()

	_, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	port, err := r.AddPort(ctx, "sb1", 3000)
	if err != nil {
		t.Fatalf("expose: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	if _, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create must not require a port: %v", err)
	}

	second, err := r.AddPort(ctx, "sb2", 8000)
	if err != nil {
		t.Fatalf("second explicit port: %v", err)
	}
	if second == port {
		t.Fatalf("explicit mapping reused hibernated port %d", port)
	}
	if _, err := r.AddPort(ctx, "sb2", 9000); err == nil {
		t.Fatal("third explicit mapping should exhaust the two-port pool")
	}
}

func TestURLOnlyPortConsumesNoHostPort(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix: "fc", TapMax: 2,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11",
		PortMin: 5200, PortMax: 5200,
	})
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := r.Create(ctx, id, "", "/tmp/"+id, nil, "", 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	pm, err := r.AddURLPort(ctx, "a", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if pm.HostPort != 0 || pm.Mode != "url" {
		t.Fatalf("URL mapping = %+v", pm)
	}
	host, err := r.AddPort(ctx, "b", 8000)
	if err != nil || host != 5200 {
		t.Fatalf("host mapping after URL-only = %d, %v", host, err)
	}
	stats, err := r.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PortUsed != 1 {
		t.Fatalf("PortUsed = %d, want only the host-port row", stats.PortUsed)
	}
}

func TestAddPortUpgradesURLOnlyMapping(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.Create(ctx, "a", "", "/tmp/a", nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddURLPort(ctx, "a", 3000); err != nil {
		t.Fatal(err)
	}
	host, err := r.AddPort(ctx, "a", 3000)
	if err != nil || host == 0 {
		t.Fatalf("upgrade = %d, %v", host, err)
	}
	ports, err := r.Ports(ctx, "a")
	if err != nil || len(ports) != 1 || ports[0].HostPort != host {
		t.Fatalf("ports = %+v, %v", ports, err)
	}
}

func TestPublicPortPersistsOnExposure(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.Create(ctx, "a", "", "/tmp/a", nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddURLPort(ctx, "a", 22); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPublicPort(ctx, "a", 22, 20000); err != nil {
		t.Fatal(err)
	}
	ports, err := r.Ports(ctx, "a")
	if err != nil || len(ports) != 1 {
		t.Fatalf("ports=%+v err=%v", ports, err)
	}
	if ports[0].PublicPort != 20000 || ports[0].Mode != "raw" || ports[0].HostPort != 0 {
		t.Fatalf("raw mapping=%+v", ports[0])
	}
}

func TestLegacyPortSchemaMigratesToNullableHostAndPublicPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sandbox_ports (
		sandbox_id TEXT NOT NULL,
		guest_port INTEGER NOT NULL,
		host_port INTEGER NOT NULL,
		PRIMARY KEY (sandbox_id, guest_port)
	); CREATE UNIQUE INDEX uniq_host_port ON sandbox_ports(host_port)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, Pools{TapPrefix: "fc", TapMax: 1, GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.10", PortMin: 5200, PortMax: 5200})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows, err := r.db.Query(`PRAGMA table_info(sandbox_ports)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]int{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = notNull
	}
	if columns["host_port"] != 0 {
		t.Fatalf("host_port remains NOT NULL: %v", columns)
	}
	if _, ok := columns["public_port"]; !ok {
		t.Fatalf("public_port was not added: %v", columns)
	}
}

func TestFreeSlotsIgnoresExplicitPorts(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	free := func() int {
		t.Helper()
		n, err := r.FreeSlots(ctx)
		if err != nil {
			t.Fatalf("free slots: %v", err)
		}
		return n
	}

	if got := free(); got != 3 {
		t.Fatalf("empty registry: FreeSlots = %d, want 3", got)
	}
	if _, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := free(); got != 1 {
		t.Fatalf("2 running: FreeSlots = %d, want 1", got)
	}

	// Hibernate one: its tap/IP return to the pool.
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if got := free(); got != 2 {
		t.Fatalf("1 running + 1 hibernated: FreeSlots = %d, want 2", got)
	}

	for _, port := range []int{8000, 8001, 8002} {
		if _, err := r.AddPort(ctx, "sb2", port); err != nil {
			t.Fatalf("add port %d: %v", port, err)
		}
	}
	if got := free(); got != 2 {
		t.Fatalf("port exhaustion must not affect create capacity: FreeSlots = %d, want 2", got)
	}

	if _, err := r.Create(ctx, "sb3", "", "/tmp/sb3.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create with exhausted port pool: %v", err)
	}
}

func TestWarmSandboxConsumesCapacityButStaysUnroutedUntilClaimed(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	warm, err := r.CreateWarm(ctx, "warm-1", "/tmp/warm.ext4", "golden", 0, 0)
	if err != nil {
		t.Fatalf("create warm: %v", err)
	}
	if warm.Status != StatusPreparing {
		t.Fatalf("warm status = %q", warm.Status)
	}
	if got, _ := r.FreeSlots(ctx); got != 2 {
		t.Fatalf("warm VM must consume capacity: free=%d, want 2", got)
	}
	if routed, _ := r.ListRouted(ctx); len(routed) != 0 {
		t.Fatalf("warm VM leaked into routed inventory: %+v", routed)
	}
	if routed, free, err := r.RoutedCapacity(ctx); err != nil {
		t.Fatalf("routed capacity: %v", err)
	} else if len(routed) != 0 || free != 2 {
		t.Fatalf("routed=%+v free=%d, want hidden warm and free=2", routed, free)
	}
	if _, err := r.ClaimWarm(ctx, "", nil, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("preparing VM was claimable: %v", err)
	}
	if err := r.MarkWarmReady(ctx, warm.ID); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	expiry := time.Now().Add(time.Minute)
	if _, err := r.db.ExecContext(ctx, `UPDATE sandboxes SET created_at=? WHERE id=?`, time.Now().Add(-time.Hour).Unix(), warm.ID); err != nil {
		t.Fatalf("age warm row: %v", err)
	}
	claimedAfter := time.Now().Add(-time.Second)
	claimed, err := r.ClaimWarm(ctx, "claimed", &expiry, 42)
	if err != nil {
		t.Fatalf("claim warm: %v", err)
	}
	if claimed.ID != warm.ID || claimed.Status != StatusRunning ||
		claimed.Name != "claimed" || claimed.HibernateAfterSec != 42 {
		t.Fatalf("bad claimed row: %+v", claimed)
	}
	if claimed.CreatedAt.Before(claimedAfter) {
		t.Fatalf("claim preserved prewarm creation time: %s", claimed.CreatedAt)
	}
	if routed, _ := r.ListRouted(ctx); len(routed) != 1 || routed[0].ID != warm.ID {
		t.Fatalf("claimed VM missing from routes: %+v", routed)
	}
	if _, err := r.ClaimWarm(ctx, "", nil, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty warm pool error = %v, want sql.ErrNoRows", err)
	}
}

func TestConcurrentWarmClaimsAreUniqueAndBounded(t *testing.T) {
	r, ctx := testRegistryWithPools(t, Pools{
		TapPrefix: "fc", TapMax: 8,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.17",
		PortMin: 5200, PortMax: 5207,
	}), context.Background()
	for i := range 4 {
		id := fmt.Sprintf("warm-%d", i)
		if _, err := r.CreateWarm(ctx, id, "/tmp/"+id+".ext4", "golden", 0, 0); err != nil {
			t.Fatalf("create warm %d: %v", i, err)
		}
		if err := r.MarkWarmReady(ctx, id); err != nil {
			t.Fatalf("mark warm %d ready: %v", i, err)
		}
	}

	const contenders = 16
	results := make(chan Sandbox, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb, err := r.ClaimWarm(ctx, fmt.Sprintf("claim-%d", i), nil, 0)
			if err != nil {
				errs <- err
				return
			}
			results <- sb
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	seen := map[string]bool{}
	for sb := range results {
		if seen[sb.ID] {
			t.Fatalf("warm sandbox claimed twice: %s", sb.ID)
		}
		seen[sb.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("successful claims=%d, want 4", len(seen))
	}
	misses := 0
	for err := range errs {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("claim error = %v, want sql.ErrNoRows", err)
		}
		misses++
	}
	if misses != contenders-4 {
		t.Fatalf("misses=%d, want %d", misses, contenders-4)
	}
	if n, err := r.WarmCount(ctx); err != nil || n != 0 {
		t.Fatalf("warm count=%d err=%v, want 0", n, err)
	}
}

func TestCreateReturnsErrPoolExhausted(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	_, err := r.Create(ctx, "d", "", "/tmp/d.ext4", nil, "", 0, 0, 0)
	if err == nil {
		t.Fatal("create beyond the pool should fail")
	}
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("exhaustion must be errors.Is-able as ErrPoolExhausted; got %v", err)
	}

	for i, id := range []string{"a", "b", "c"} {
		if _, err := r.AddPort(ctx, id, 8000+i); err != nil {
			t.Fatalf("AddPort %s: %v", id, err)
		}
	}
	if _, err := r.AddPort(ctx, "a", 9000); err == nil || !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("AddPort exhaustion should wrap ErrPoolExhausted; got %v", err)
	}
}

// memRegistry is the 3-slot test registry with memory admission on: template
// 1024 MiB + 156 overhead = 1180 per sandbox, budget fits exactly two.
func memRegistry(t *testing.T) *Registry {
	t.Helper()
	r := testRegistry(t)
	r.SetMemAccounting(MemAccounting{TemplateMemMIB: 1024, BudgetMIB: 2 * 1180, OverheadMIB: 156})
	return r
}

func TestCreateRejectsBeyondMemBudget(t *testing.T) {
	r, ctx := memRegistry(t), context.Background()

	for _, id := range []string{"a", "b"} {
		if _, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s within budget: %v", id, err)
		}
	}
	_, err := r.Create(ctx, "c", "", "/tmp/c.ext4", nil, "", 0, 0, 0)
	if err == nil {
		t.Fatal("third template create should exceed the 2-sandbox memory budget")
	}
	if !errors.Is(err, ErrMemExhausted) || !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("rejection must be Is-able as BOTH ErrMemExhausted and ErrPoolExhausted (503/failover path); got %v", err)
	}
}

func TestCreateMixedOverridesAgainstBudget(t *testing.T) {
	r, ctx := memRegistry(t), context.Background()

	// One big override eats the whole budget (2204 + 156 = 2360 = budget).
	if _, err := r.Create(ctx, "big", "", "/tmp/big.ext4", nil, "", 0, 0, 2204); err != nil {
		t.Fatalf("big create exactly at budget: %v", err)
	}
	if _, err := r.Create(ctx, "small", "", "/tmp/small.ext4", nil, "", 0, 0, 0); err == nil {
		t.Fatal("template create should be rejected: the override consumed the budget")
	}
	// Freeing the big one re-admits.
	if err := r.Destroy(ctx, "big"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := r.Create(ctx, "small", "", "/tmp/small.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create after destroy should be admitted: %v", err)
	}
}

func TestHibernatedHoldsNoMemoryAndWakeRecommits(t *testing.T) {
	r, ctx := memRegistry(t), context.Background()

	if _, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create sb1: %v", err)
	}
	if _, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create sb2: %v", err)
	}
	// Budget full. Hibernating sb1 releases its memory (the VM is dead)...
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if _, err := r.Create(ctx, "sb3", "", "/tmp/sb3.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create sb3 should fit — hibernated sandboxes hold no memory: %v", err)
	}
	// ...but waking re-commits it, and the budget is full again.
	if _, _, err := r.Wake(ctx, "sb1"); err == nil {
		t.Fatal("wake should be rejected: re-committing sb1's memory exceeds the budget")
	} else if !errors.Is(err, ErrMemExhausted) {
		t.Fatalf("wake rejection should be ErrMemExhausted; got %v", err)
	}
	// Rejection must leave the row hibernated (wakeable later).
	sb, err := r.Get(ctx, "sb1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sb.Status != StatusHibernated {
		t.Fatalf("rejected wake must keep the row hibernated; got %s", sb.Status)
	}
	if err := r.Destroy(ctx, "sb3"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, _, err := r.Wake(ctx, "sb1"); err != nil {
		t.Fatalf("wake after freeing capacity: %v", err)
	}
}

func TestFreeSlotsMemoryBound(t *testing.T) {
	r, ctx := memRegistry(t), context.Background()

	// Pools allow 3, the budget fits 2 → memory is the binding term.
	if got, err := r.FreeSlots(ctx); err != nil || got != 2 {
		t.Fatalf("FreeSlots = %d, %v; want 2 (memory-bound)", got, err)
	}
	// Disabled budget → pool-bound as before.
	r.SetMemAccounting(MemAccounting{})
	if got, err := r.FreeSlots(ctx); err != nil || got != 3 {
		t.Fatalf("FreeSlots with admission disabled = %d, %v; want 3", got, err)
	}
}

func TestRestoreChargesBakedMem(t *testing.T) {
	r, ctx := memRegistry(t), context.Background()

	// A restore whose snapshot baked more memory than the budget allows.
	_, err := r.CreateRestore(ctx, "big", "", "/tmp/big.ext4", "fc0", "172.16.0.10", nil, 0, 0, 4096)
	if err == nil {
		t.Fatal("restore with baked mem beyond the budget should be rejected")
	}
	if !errors.Is(err, ErrMemExhausted) {
		t.Fatalf("restore rejection should be ErrMemExhausted; got %v", err)
	}
}

func TestMigrationRetiresLegacyPrimaryPorts(t *testing.T) {
	dir := t.TempDir()
	pools := Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	}
	r, err := Open(filepath.Join(dir, "registry.db"), pools)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	ctx := context.Background()
	if _, err := r.Create(ctx, "hib", "", "/tmp/hib.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "hib"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if _, err := r.Create(ctx, "run", "", "/tmp/run.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.db.Exec(`ALTER TABLE sandboxes ADD COLUMN host_port INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatalf("add legacy column: %v", err)
	}
	if _, err := r.db.Exec(`UPDATE sandboxes SET host_port=5200`); err != nil {
		t.Fatalf("inject legacy primary ports: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := Open(filepath.Join(dir, "registry.db"), pools)
	if err != nil {
		t.Fatalf("reopen after legacy ports: %v", err)
	}
	defer r2.Close()
	rows, err := r2.db.Query(`PRAGMA table_info(sandboxes)`)
	if err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "host_port" {
			t.Fatal("migration left the legacy host_port column in place")
		}
	}
}

func TestHibernateAfterSecPersistsThroughLifecycle(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 60, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.HibernateAfterSec != 60 {
		t.Fatalf("create should record the override, got %d", sb.HibernateAfterSec)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	woken, _, err := r.Wake(ctx, "sb1")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if woken.HibernateAfterSec != 60 {
		t.Fatalf("override must survive hibernate/wake, got %d", woken.HibernateAfterSec)
	}

	// -1 (never hibernate) round-trips too.
	never, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", -1, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := r.Get(ctx, never.ID); err != nil || got.HibernateAfterSec != -1 {
		t.Fatalf("get after create: %v, hibernate_after_sec=%d want -1", err, got.HibernateAfterSec)
	}
}

func TestResourceOverridesPersist(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 4, 2048)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.Vcpus != 4 || sb.MemMIB != 2048 {
		t.Fatalf("create should record resource overrides, got vcpus=%d mem_mib=%d", sb.Vcpus, sb.MemMIB)
	}
	if got, err := r.Get(ctx, "sb1"); err != nil || got.Vcpus != 4 || got.MemMIB != 2048 {
		t.Fatalf("get after create: %v, vcpus=%d mem_mib=%d want 4/2048", err, got.Vcpus, got.MemMIB)
	}

	// The overrides survive hibernate/wake — the wake path must not fall back
	// to template defaults (the hibernation snapshot bakes the real resources).
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	woken, _, err := r.Wake(ctx, "sb1")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if woken.Vcpus != 4 || woken.MemMIB != 2048 {
		t.Fatalf("overrides must survive hibernate/wake, got vcpus=%d mem_mib=%d", woken.Vcpus, woken.MemMIB)
	}

	// Absent overrides read back as 0 (= template default).
	plain, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plain.Vcpus != 0 || plain.MemMIB != 0 {
		t.Fatalf("no-override sandbox should report 0/0, got vcpus=%d mem_mib=%d", plain.Vcpus, plain.MemMIB)
	}
}

func TestSnapshotRecordsSourceResources(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	// Snapshot rows carry the source's baked resources...
	snap := Snapshot{
		ID: "snap1", SourceID: "sb1", TapDevice: "fc0", GuestIP: "172.16.0.10",
		GuestMAC: "02:aa:bb:cc:dd:ee",
		MemPath:  "/tmp/mem", StatePath: "/tmp/state", RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt: time.Now(), Vcpus: 4, MemMIB: 2048,
	}
	if err := r.CreateSnapshot(ctx, snap); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	got, err := r.GetSnapshot(ctx, "snap1")
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.Vcpus != 4 || got.MemMIB != 2048 {
		t.Fatalf("snapshot must record source resources, got vcpus=%d mem_mib=%d", got.Vcpus, got.MemMIB)
	}
	if got.GuestMAC != snap.GuestMAC {
		t.Fatalf("snapshot guest MAC = %q, want %q", got.GuestMAC, snap.GuestMAC)
	}

	// ...and a restore stamps them onto the new row.
	sb, err := r.CreateRestore(ctx, "sb2", "", "/tmp/sb2.ext4", got.TapDevice, got.GuestIP, nil, 0, got.Vcpus, got.MemMIB)
	if err != nil {
		t.Fatalf("create restore: %v", err)
	}
	if sb.Vcpus != 4 || sb.MemMIB != 2048 {
		t.Fatalf("restored row must report the snapshot's resources, got vcpus=%d mem_mib=%d", sb.Vcpus, sb.MemMIB)
	}
}

func TestExpiredIncludesHibernated(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	past := time.Now().Add(-time.Minute)
	if _, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", &past, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	expired, err := r.Expired(ctx, time.Now())
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "sb1" {
		t.Fatalf("hibernated sandbox past its TTL should be reaped, got %+v", expired)
	}
}

func TestListRoutedAndListSplitStatuses(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	if _, err := r.Create(ctx, "run1", "", "/tmp/r1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.Create(ctx, "hib1", "", "/tmp/h1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "hib1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	running, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(running) != 1 || running[0].ID != "run1" {
		t.Fatalf("List should return only running sandboxes, got %+v", running)
	}
	routed, err := r.ListRouted(ctx)
	if err != nil {
		t.Fatalf("list routed: %v", err)
	}
	if len(routed) != 2 {
		t.Fatalf("ListRouted should include hibernated, got %+v", routed)
	}
}

func TestRoutedCapacityUsesSameRowsForUsedAndFree(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	r.SetMemAccounting(MemAccounting{
		TemplateMemMIB: 1024,
		OverheadMIB:    156,
		BudgetMIB:      2 * 1180,
	})

	if _, err := r.Create(ctx, "run1", "", "/tmp/r1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create running: %v", err)
	}
	if _, err := r.Create(ctx, "hib1", "", "/tmp/h1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create hibernated: %v", err)
	}
	if err := r.Hibernate(ctx, "hib1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	routed, free, err := r.RoutedCapacity(ctx)
	if err != nil {
		t.Fatalf("routed capacity: %v", err)
	}
	if len(routed) != 2 {
		t.Fatalf("routed rows = %d, want running + hibernated = 2", len(routed))
	}
	running := 0
	for _, sb := range routed {
		if sb.Status == StatusRunning {
			running++
		}
	}
	if running != 1 || free != 1 {
		t.Fatalf("coherent used/free = %d/%d, want 1/1 (memory-bound)", running, free)
	}
}

func TestRoutedCapacityAvoidsDeleteBetweenHeartbeatReads(t *testing.T) {
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     48,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.57",
		PortMin:    5200,
		PortMax:    5247,
	})
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("sb-%d", i)
		if _, err := r.Create(ctx, id, "", "/tmp/"+id+".ext4", nil, "", 0, 0, 0); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// Deterministically reproduce the old heartbeat's two-query race: list
	// first, then let five deletes commit before querying free capacity.
	oldRouted, err := r.ListRouted(ctx)
	if err != nil {
		t.Fatalf("old list routed: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := r.Destroy(ctx, fmt.Sprintf("sb-%d", i)); err != nil {
			t.Fatalf("destroy %d: %v", i, err)
		}
	}
	oldFree, err := r.FreeSlots(ctx)
	if err != nil {
		t.Fatalf("old free slots: %v", err)
	}
	if len(oldRouted) != 7 || oldFree != 46 || len(oldRouted)+oldFree != 53 {
		t.Fatalf("failed to reproduce old skew: used=%d free=%d", len(oldRouted), oldFree)
	}

	// The heartbeat API performs one routed-row read and derives capacity from
	// those exact rows, so no delete can split its used/free snapshot.
	routed, free, err := r.RoutedCapacity(ctx)
	if err != nil {
		t.Fatalf("routed capacity: %v", err)
	}
	if len(routed) != 2 || free != 46 || len(routed)+free != 48 {
		t.Fatalf("coherent snapshot used/free = %d/%d, want 2/46", len(routed), free)
	}
}

func TestNamesPersistAndRename(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "my devbox", "/tmp/sb1.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.Name != "my devbox" {
		t.Fatalf("create must return the name, got %q", sb.Name)
	}
	got, err := r.Get(ctx, "sb1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "my devbox" {
		t.Fatalf("name must persist, got %q", got.Name)
	}

	if err := r.SetName(ctx, "sb1", "renamed"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	if got, _ = r.Get(ctx, "sb1"); got.Name != "renamed" {
		t.Fatalf("rename must persist, got %q", got.Name)
	}
	if err := r.SetName(ctx, "sb1", ""); err != nil {
		t.Fatalf("clear name: %v", err)
	}
	if got, _ = r.Get(ctx, "sb1"); got.Name != "" {
		t.Fatalf("empty name must clear, got %q", got.Name)
	}
	if err := r.SetName(ctx, "nope", "x"); err == nil {
		t.Fatal("SetName on unknown id must fail")
	}

	snap := Snapshot{
		ID: "snap1", Name: "golden-ish", SourceID: "sb1", TapDevice: "fc0", GuestIP: "172.16.0.10",
		MemPath: "/tmp/mem", StatePath: "/tmp/state", RootfsPath: "/tmp/rootfs.ext4",
		CreatedAt: time.Now(),
	}
	if err := r.CreateSnapshot(ctx, snap); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if got, _ := r.GetSnapshot(ctx, "snap1"); got.Name != "golden-ish" {
		t.Fatalf("snapshot name must persist, got %q", got.Name)
	}
	if err := r.SetSnapshotName(ctx, "snap1", "prepped"); err != nil {
		t.Fatalf("set snapshot name: %v", err)
	}
	if got, _ := r.GetSnapshot(ctx, "snap1"); got.Name != "prepped" {
		t.Fatalf("snapshot rename must persist, got %q", got.Name)
	}
	if err := r.SetSnapshotName(ctx, "nope", "x"); err == nil {
		t.Fatal("SetSnapshotName on unknown id must fail")
	}
}

func TestSandboxPublicFieldsRoundTrip(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	sb, err := r.Create(ctx, "sb-public", "initial", "/tmp/sb-public.ext4", nil, "", 0, 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	sb, err = r.SetPublicFields(ctx, sb.ID, "snapshot", "snap_123", map[string]string{"run_id": "ci_456"})
	if err != nil {
		t.Fatal(err)
	}
	if sb.SourceType != "snapshot" || sb.SourceID != "snap_123" || sb.Metadata["run_id"] != "ci_456" {
		t.Fatalf("public fields = %+v", sb)
	}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	sb, err = r.UpdatePublicFields(ctx, sb.ID, "renamed", map[string]string{"team": "runtime"}, &expiry, 300)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name != "renamed" || sb.Metadata["team"] != "runtime" || sb.HibernateAfterSec != 300 ||
		sb.ExpiresAt == nil || !sb.ExpiresAt.Equal(expiry) {
		t.Fatalf("updated public fields = %+v", sb)
	}
	if _, err := r.UpdatePublicFields(ctx, "missing", "", nil, nil, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing update error = %v", err)
	}
}

func TestSnapshotLifecycleAndDependencyProtection(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	now := time.Now().Truncate(time.Second)
	snap := Snapshot{
		ID: "snap-public", SourceID: "source", TapDevice: "fc0", GuestIP: "172.16.0.10",
		MemPath: "/tmp/mem", StatePath: "/tmp/state", RootfsPath: "/tmp/rootfs",
		CreatedAt: now,
	}
	if err := r.CreateSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	expiry := now.Add(time.Minute)
	got, err := r.SetSnapshotPublicFields(ctx, snap.ID, "release-candidate", &expiry)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "release-candidate" || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiry) || got.Durability != "local" {
		t.Fatalf("snapshot fields = %+v", got)
	}
	if err := r.SetSnapshotDurability(ctx, snap.ID, "durable"); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetSnapshot(ctx, snap.ID)
	if got.Durability != "durable" {
		t.Fatalf("durability = %q", got.Durability)
	}
	if expired, err := r.ExpiredSnapshots(ctx, expiry.Add(time.Second)); err != nil || len(expired) != 1 {
		t.Fatalf("expired = %+v, %v", expired, err)
	}

	sb, err := r.Create(ctx, "dependent", "", "/tmp/dependent", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetPublicFields(ctx, sb.ID, "snapshot", snap.ID, nil); err != nil {
		t.Fatal(err)
	}
	if dependencies, err := r.SnapshotDependencyCount(ctx, snap.ID); err != nil || dependencies != 1 {
		t.Fatalf("dependency count = %d, %v; want 1", dependencies, err)
	}
	if err := r.DeleteSnapshot(ctx, snap.ID); !errors.Is(err, ErrSnapshotInUse) {
		t.Fatalf("dependent delete error = %v", err)
	}
	if err := r.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if dependencies, err := r.SnapshotDependencyCount(ctx, snap.ID); err != nil || dependencies != 0 {
		t.Fatalf("dependency count after destroy = %d, %v; want 0", dependencies, err)
	}
	if err := r.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("delete after dependent removal: %v", err)
	}
}

func TestStartingSandboxIsHiddenUntilReady(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	sb, err := r.CreateStarting(ctx, "starting", "", "/tmp/starting", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusStarting {
		t.Fatalf("status = %q, want %q", sb.Status, StatusStarting)
	}
	routed, err := r.ListRouted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(routed) != 0 {
		t.Fatalf("starting sandbox was routed: %+v", routed)
	}
	if free, err := r.FreeSlots(ctx); err != nil || free != r.pools.TapMax-1 {
		t.Fatalf("free slots while starting = %d, %v", free, err)
	}
	if err := r.FinishStart(ctx, sb.ID, 123, "vm", "/tmp/socket"); err != nil {
		t.Fatal(err)
	}
	if routed, _ := r.ListRouted(ctx); len(routed) != 0 {
		t.Fatalf("FinishStart published sandbox before readiness: %+v", routed)
	}
	if err := r.MarkRunning(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	routed, err = r.ListRouted(ctx)
	if err != nil || len(routed) != 1 || routed[0].Status != StatusRunning {
		t.Fatalf("routed after MarkRunning = %+v, %v", routed, err)
	}
}

func TestStoppingSandboxIsUnroutedButKeepsCapacity(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	sb, err := r.Create(ctx, "stopping", "", "/tmp/stopping", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkStopping(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if routed, err := r.ListRouted(ctx); err != nil || len(routed) != 0 {
		t.Fatalf("stopping sandbox routed = %+v, %v", routed, err)
	}
	if free, err := r.FreeSlots(ctx); err != nil || free != r.pools.TapMax-1 {
		t.Fatalf("free slots while stopping = %d, %v", free, err)
	}
	if err := r.Destroy(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if free, err := r.FreeSlots(ctx); err != nil || free != r.pools.TapMax {
		t.Fatalf("free slots after destroy = %d, %v", free, err)
	}
}
