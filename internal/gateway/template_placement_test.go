package gateway

import (
	"testing"
	"time"
)

func TestTemplateWarmAffinityOutranksSnapshotLocality(t *testing.T) {
	g := New("token", time.Minute, 0, 0)
	now := time.Now()
	g.hosts["owner"] = &host{id: "owner", slotsTotal: 8, slotsFree: 4, lastSeen: now}
	g.hosts["warm"] = &host{
		id: "warm", slotsTotal: 8, slotsFree: 6, lastSeen: now,
		warmReady: 2, warmReadyByTemplate: map[string]int{"template-a": 2},
	}

	got := g.reserveHostForTemplate(nil, 2, "owner", "template-a")
	if got == nil || got.id != "warm" {
		t.Fatalf("placement = %#v, want template-warm host", got)
	}
	if got.reservationWarmCount != 2 || !got.reservationWarm {
		t.Fatalf("warm reservation = %d/%v, want 2/true", got.reservationWarmCount, got.reservationWarm)
	}
}

func TestTemplatePlacementNeverUsesWrongWarmPool(t *testing.T) {
	g := New("token", time.Minute, 0, 0)
	now := time.Now()
	g.hosts["wrong"] = &host{
		id: "wrong", slotsTotal: 8, slotsFree: 2, lastSeen: now,
		warmReady: 4, warmReadyByTemplate: map[string]int{"template-b": 4},
	}
	g.hosts["ordinary"] = &host{id: "ordinary", slotsTotal: 8, slotsFree: 3, lastSeen: now}

	got := g.reserveHostForTemplate(nil, 1, "", "template-a")
	if got == nil || got.id != "wrong" { // normal bin-pack is still allowed
		t.Fatalf("placement = %#v, want fullest ordinary-capacity host", got)
	}
	if got.reservationWarm {
		t.Fatal("wrong template inventory was reserved as a warm match")
	}
}
