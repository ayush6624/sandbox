package server

import (
	"encoding/json"
	"testing"
)

func TestNextOwnerFromAbsent(t *testing.T) {
	// No prior fence → the first claimant starts at epoch 1.
	got := nextOwner(nil, "host-A")
	if got.HostID != "host-A" || got.Epoch != 1 {
		t.Fatalf("first claim = %+v, want {host-A 1}", got)
	}
}

func TestNextOwnerBumpsEpoch(t *testing.T) {
	prev, _ := json.Marshal(hibOwner{HostID: "host-A", Epoch: 7})
	got := nextOwner(prev, "host-B")
	if got.HostID != "host-B" {
		t.Fatalf("owner = %q, want host-B", got.HostID)
	}
	// Strictly increasing so a stale writer can never re-establish an old claim.
	if got.Epoch != 8 {
		t.Fatalf("epoch = %d, want 8 (strictly > prior 7)", got.Epoch)
	}
}

func TestNextOwnerCorruptFenceTreatedAsZero(t *testing.T) {
	// A garbage fence must not wedge ownership: treat it as epoch 0 → claim at 1.
	got := nextOwner([]byte("not json"), "host-C")
	if got.HostID != "host-C" || got.Epoch != 1 {
		t.Fatalf("corrupt-fence claim = %+v, want {host-C 1}", got)
	}
}
