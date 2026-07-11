package server

import (
	"testing"
	"time"
)

func TestActivityTrackerPinsInflightRequests(t *testing.T) {
	a := newActivityTracker()

	// Unknown id: the reaper must treat it as "never seen" and seed it.
	if _, _, ok := a.idleFor("sb"); ok {
		t.Fatal("unknown id should report ok=false")
	}

	// An in-flight request pins the sandbox busy no matter how old its last
	// touch is — this is what keeps open shells and exec streams alive.
	done := a.begin("sb")
	if _, busy, ok := a.idleFor("sb"); !ok || !busy {
		t.Fatalf("in-flight request must report busy (ok=%v busy=%v)", ok, busy)
	}
	// Overlapping requests: still busy until the LAST one finishes.
	done2 := a.begin("sb")
	done()
	if _, busy, _ := a.idleFor("sb"); !busy {
		t.Fatal("one of two overlapping requests finished; must still be busy")
	}
	done2()
	idle, busy, ok := a.idleFor("sb")
	if !ok || busy {
		t.Fatalf("all requests done; must be idle (ok=%v busy=%v)", ok, busy)
	}
	// done() touches, so the idle clock starts at request END.
	if idle > time.Second {
		t.Fatalf("idle clock should have just restarted, got %s", idle)
	}
}

func TestActivityTrackerForget(t *testing.T) {
	a := newActivityTracker()
	a.touch("sb")
	if _, _, ok := a.idleFor("sb"); !ok {
		t.Fatal("touched id must be tracked")
	}
	a.forget("sb")
	if _, _, ok := a.idleFor("sb"); ok {
		t.Fatal("forgotten id must be untracked")
	}
}

func TestActivityTrackerDoneIsIdempotentPerRequest(t *testing.T) {
	a := newActivityTracker()
	done := a.begin("sb")
	done()
	// A second begin/done cycle must not underflow the inflight count into
	// permanently-busy or negative territory.
	done2 := a.begin("sb")
	if _, busy, _ := a.idleFor("sb"); !busy {
		t.Fatal("second request must pin busy again")
	}
	done2()
	if _, busy, _ := a.idleFor("sb"); busy {
		t.Fatal("must be idle after the second request ends")
	}
}
