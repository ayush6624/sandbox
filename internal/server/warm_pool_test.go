package server

import (
	"context"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func TestAllocateWarmPoolTargetsIsPerTemplateAndBudgetBounded(t *testing.T) {
	s, reg := capacityTestServer(t)
	s.cfg.WarmPoolSize = 1
	s.cfg.WarmPoolBudget = 2
	ctx := context.Background()
	now := time.Now()
	for _, snap := range []registry.Snapshot{
		{ID: "builtin", Golden: true, Role: registry.SnapshotRoleBuiltin, WarmTarget: 99, CreatedAt: now.Add(-3 * time.Second)},
		{ID: "template-new", Role: registry.SnapshotRoleTemplate, WarmTarget: 3, CreatedAt: now.Add(-time.Second)},
		{ID: "template-old", Role: registry.SnapshotRoleTemplate, WarmTarget: 3, CreatedAt: now.Add(-2 * time.Second)},
		{ID: "user-snapshot", Role: registry.SnapshotRoleUser, WarmTarget: 9, CreatedAt: now},
	} {
		if err := reg.CreateSnapshot(ctx, snap); err != nil {
			t.Fatalf("create snapshot %s: %v", snap.ID, err)
		}
	}

	targets, err := s.warmPoolTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want builtin plus one budget-clamped template", targets)
	}
	if targets[0].ID != "builtin" || targets[0].WarmTarget != 1 {
		t.Fatalf("builtin target = %#v, want builtin/1", targets[0])
	}
	if targets[1].ID != "template-new" || targets[1].WarmTarget != 1 {
		t.Fatalf("custom target = %#v, want newest template clamped to remaining budget 1", targets[1])
	}
	if targets[0].WarmTarget+targets[1].WarmTarget > s.cfg.WarmPoolBudget {
		t.Fatalf("allocated targets exceed budget: %#v", targets)
	}
}
