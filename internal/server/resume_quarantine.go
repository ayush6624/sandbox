package server

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// A normal heartbeat is every five seconds. Crossing three intervals is
	// already close to the gateway's stale-host TTL and reliably distinguishes
	// a suspended VM from ordinary ticker jitter.
	heartbeatWakeGap = 3 * heartbeatInterval

	// Nomad 1.7 can reconnect a suspended raw_exec allocation, then fail its
	// delayed executor relaunch with EBUSY. In the observed incident that
	// decision arrived 18 seconds after the worker returned. Keep admission
	// closed past that window; a replacement allocation can still register and
	// route, but the old process cannot receive work that Nomad may then kill.
	resumePlacementQuarantine = 30 * time.Second
)

type heartbeatWakeState struct {
	mu             sync.Mutex
	lastWallNanos  int64
	untilWallNanos int64
}

// heartbeatWakeStates is process-local and normally has one entry. Keeping the
// resume detector outside Server avoids coupling the lifecycle-critical gate
// to the much larger server state; the process owns both for the same lifetime.
var heartbeatWakeStates sync.Map // *Server -> *heartbeatWakeState

// noteHeartbeatWake records an attempted heartbeat and reports whether create
// placement must remain quarantined. It deliberately uses wall time: Go's
// monotonic clock and the process are frozen during GCE suspend, while wall
// time advances, which is the signal we need here.
func (s *Server) noteHeartbeatWake(now time.Time) bool {
	v, _ := heartbeatWakeStates.LoadOrStore(s, &heartbeatWakeState{})
	state := v.(*heartbeatWakeState)

	state.mu.Lock()
	defer state.mu.Unlock()

	nowWall := now.UnixNano() // strips Go's suspend-excluding monotonic component
	gap := time.Duration(nowWall - state.lastWallNanos)
	if state.lastWallNanos != 0 && gap >= heartbeatWakeGap {
		candidate := nowWall + int64(resumePlacementQuarantine)
		if candidate > state.untilWallNanos {
			state.untilWallNanos = candidate
			fmt.Fprintf(os.Stderr, "heartbeat: detected %s scheduling gap; quarantining create placement for %s\n",
				gap.Round(time.Second), resumePlacementQuarantine)
		}
	}
	state.lastWallNanos = nowWall
	return nowWall < state.untilWallNanos
}
