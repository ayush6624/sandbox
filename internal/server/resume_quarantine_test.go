package server

import (
	"testing"
	"time"
)

func TestHeartbeatWakeQuarantinesPlacementAfterSchedulingGap(t *testing.T) {
	s := &Server{}
	t.Cleanup(func() { heartbeatWakeStates.Delete(s) })

	t0 := time.Unix(1_700_000_000, 0)
	if s.noteHeartbeatWake(t0) {
		t.Fatal("first heartbeat was quarantined")
	}
	if s.noteHeartbeatWake(t0.Add(heartbeatInterval)) {
		t.Fatal("normal heartbeat interval was quarantined")
	}

	wake := t0.Add(heartbeatInterval + heartbeatWakeGap)
	if !s.noteHeartbeatWake(wake) {
		t.Fatal("heartbeat after suspended scheduling gap was not quarantined")
	}
	for elapsed := heartbeatInterval; elapsed < resumePlacementQuarantine; elapsed += heartbeatInterval {
		if !s.noteHeartbeatWake(wake.Add(elapsed)) {
			t.Fatalf("resume quarantine opened early after %s", elapsed)
		}
	}
	if s.noteHeartbeatWake(wake.Add(resumePlacementQuarantine)) {
		t.Fatal("resume quarantine remained closed after deadline")
	}
}

func TestHeartbeatWakeIgnoresBackwardWallClockStep(t *testing.T) {
	s := &Server{}
	t.Cleanup(func() { heartbeatWakeStates.Delete(s) })

	now := time.Unix(1_700_000_000, 0)
	if s.noteHeartbeatWake(now) || s.noteHeartbeatWake(now.Add(-time.Minute)) {
		t.Fatal("backward wall-clock step triggered resume quarantine")
	}
}
