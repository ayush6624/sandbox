package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry.db"), Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestHibernateFreesSlotAndWakeReclaimsIdentity(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "/tmp/sb1.ext4", nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	// The hibernated identity is free but SOFT-avoided: a new sandbox must
	// pick different resources while the pool has other entries.
	other, err := r.Create(ctx, "sb2", "/tmp/sb2.ext4", nil, "")
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
	r, ctx := testRegistry(t), context.Background()

	sb, err := r.Create(ctx, "sb1", "/tmp/sb1.ext4", nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Hibernate(ctx, "sb1"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	// Fill the whole pool (3 slots): the last create is forced onto the
	// hibernated identity — soft avoidance yields when the pool is exhausted.
	squatted := false
	for _, id := range []string{"a", "b", "c"} {
		got, err := r.Create(ctx, id, "/tmp/"+id+".ext4", nil, "")
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

	// No capacity at all → wake must fail (host is full).
	if _, _, err := r.Wake(ctx, "sb1"); err == nil {
		t.Fatal("wake with an exhausted pool should fail")
	}

	// Free one slot; wake must succeed with a FRESH identity.
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

func TestExpiredIncludesHibernated(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()

	past := time.Now().Add(-time.Minute)
	if _, err := r.Create(ctx, "sb1", "/tmp/sb1.ext4", &past, ""); err != nil {
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

	if _, err := r.Create(ctx, "run1", "/tmp/r1.ext4", nil, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.Create(ctx, "hib1", "/tmp/h1.ext4", nil, ""); err != nil {
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
