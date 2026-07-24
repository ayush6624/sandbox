package registry

import (
	"context"
	"errors"
	"path/filepath"
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
		MemPath: "/tmp/mem", StatePath: "/tmp/state", RootfsPath: "/tmp/rootfs.ext4",
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
