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
	if other.TapDevice == sb.TapDevice || other.GuestIP == sb.GuestIP || other.HostPort == sb.HostPort {
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
	if woken.TapDevice != sb.TapDevice || woken.GuestIP != sb.GuestIP || woken.HostPort != sb.HostPort {
		t.Fatalf("same-identity wake changed resources: %+v vs %+v", woken, sb)
	}
	if woken.Status != StatusRunning || woken.StoppedAt != nil {
		t.Fatalf("woken sandbox should be running with no stopped_at: %+v", woken)
	}
}

func TestWakeAllocatesFreshIdentityWhenSquatted(t *testing.T) {
	// One extra port vs taps/IPs: the hibernated sandbox's port stays
	// hard-reserved, so filling every tap needs a port pool one bigger.
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5203,
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
		if got.HostPort == sb.HostPort {
			t.Fatalf("create %s squatted the hibernated HOST PORT %d — ports must stay hard-reserved", id, sb.HostPort)
		}
	}
	if !squatted {
		t.Fatal("filling the pool should have forced a create onto the hibernated tap")
	}

	// No tap capacity at all → wake must fail (host is full).
	if _, _, err := r.Wake(ctx, "sb1"); err == nil {
		t.Fatal("wake with an exhausted pool should fail")
	}

	// Free one slot; wake must succeed with a FRESH tap/IP but the SAME host
	// port — the wake-on-connect listener never let go of it.
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
	if woken.HostPort != sb.HostPort {
		t.Fatalf("wake must keep the hard-reserved host port: got %d want %d", woken.HostPort, sb.HostPort)
	}
	if woken.Status != StatusRunning {
		t.Fatalf("woken sandbox should be running: %+v", woken)
	}
}

func TestHibernatedPortStaysReserved(t *testing.T) {
	// 2 ports but 3 taps/IPs: port exhaustion hits first, proving hibernated
	// ports are excluded from the pool outright (not just soft-avoided).
	r := testRegistryWithPools(t, Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5201,
	})
	ctx := context.Background()

	sb, err := r.Create(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	sb2, err := r.Create(ctx, "sb2", "", "/tmp/sb2.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb2.HostPort == sb.HostPort {
		t.Fatalf("sb2 took the hibernated sandbox's port %d", sb.HostPort)
	}

	// Both a create and an extra-port expose must fail: the only port left in
	// the pool belongs to the hibernated sandbox.
	if _, err := r.Create(ctx, "sb3", "", "/tmp/sb3.ext4", nil, "", 0, 0, 0); err == nil {
		t.Fatal("create should fail with the pool's last port held by a hibernated sandbox")
	}
	if _, err := r.AddPort(ctx, "sb2", 8000); err == nil {
		t.Fatal("AddPort should fail with the pool's last port held by a hibernated sandbox")
	}

	// Wake keeps the reserved port even with sb2 running alongside.
	woken, _, err := r.Wake(ctx, "sb1")
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if woken.HostPort != sb.HostPort {
		t.Fatalf("wake changed the host port: got %d want %d", woken.HostPort, sb.HostPort)
	}
}

func TestFreeSlotsAccountsHibernatedPortsAndExtraPorts(t *testing.T) {
	// Ports = taps = IPs = 3. Hibernated sandboxes free their tap/IP but hold
	// their port, so FreeSlots must be port-bound while Slots()-running lies.
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

	// Hibernate one: tap/IP return to the pool but the port stays held.
	// Slots()-running would now claim 2 free; the truth is still 1.
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if got, lie := free(), r.Pools().Slots()-1; got != 1 || lie != 2 {
		t.Fatalf("1 running + 1 hibernated: FreeSlots = %d (want 1); Slots()-running = %d (the overstatement this fixes)", got, lie)
	}

	// An extra exposed port drains the same pool: nothing left to create with.
	if _, err := r.AddPort(ctx, "sb2", 8000); err != nil {
		t.Fatalf("add port: %v", err)
	}
	if got := free(); got != 0 {
		t.Fatalf("extra port should exhaust the port pool: FreeSlots = %d, want 0", got)
	}

	// Destroying the hibernated sandbox releases its port.
	if err := r.Destroy(ctx, "sb1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := free(); got != 1 {
		t.Fatalf("after destroy: FreeSlots = %d, want 1", got)
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

	// AddPort exhaustion carries the sentinel too (ports are the create-path
	// pool the gateway needs to recognize as capacity).
	if _, err := r.AddPort(ctx, "a", 8000); err == nil || !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("AddPort exhaustion should wrap ErrPoolExhausted; got %v", err)
	}
}

func TestMigrationDedupsCollidingHibernatedPorts(t *testing.T) {
	// Simulate a pre-proxy database: a hibernated row sharing its host port
	// with a running row (soft reservation allowed that under pool pressure).
	// Reopening the registry must move the hibernated row to a free port so
	// the strict uniq_port_held index can be created.
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
	// Force the pre-upgrade collision by hand: revert to the old running-only
	// index (uniq_port_held would refuse the duplicate), then collide.
	if _, err := r.db.Exec(`DROP INDEX uniq_port_held`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := r.db.Exec(`UPDATE sandboxes SET host_port=(SELECT host_port FROM sandboxes WHERE id='run') WHERE id='hib'`); err != nil {
		t.Fatalf("inject collision: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r2, err := Open(filepath.Join(dir, "registry.db"), pools)
	if err != nil {
		t.Fatalf("reopen after collision must dedup, got: %v", err)
	}
	defer r2.Close()
	hib, err := r2.Get(ctx, "hib")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	run, err := r2.Get(ctx, "run")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if hib.HostPort == run.HostPort {
		t.Fatalf("migration left the port collision in place: both on %d", hib.HostPort)
	}
	if hib.HostPort < pools.PortMin || hib.HostPort > pools.PortMax {
		t.Fatalf("dedup picked a port outside the pool: %d", hib.HostPort)
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
