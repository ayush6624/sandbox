package server

import (
	"context"
	"errors"
	"testing"

	"github.com/ayush6624/sandbox/internal/vm"
)

func TestFailedHandoffReopensOnlyAfterConfirmedRollback(t *testing.T) {
	for _, kind := range []string{"adoption", "wake"} {
		for _, state := range []string{"confirmed", "live_machine", "wrong_row", "read_failure"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				store := newUploadTestStore(t)
				s := handoffTestServer(t, store, "owner")
				observer := handoffTestServer(t, store, "observer")
				id := "failed-handoff"
				seedHandoffOffer(t, s, id)
				claim, err := s.claimHandoff(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.authorizeHandoffRun(ctx, claim); err != nil {
					t.Fatal(err)
				}
				if _, err := s.reg.Create(ctx, id, "", "/tmp/unused-handoff-rootfs", nil, "", 0, 1, 1); err != nil {
					t.Fatal(err)
				}
				if state != "wrong_row" {
					if kind == "adoption" {
						err = s.reg.Destroy(ctx, id)
					} else {
						err = s.reg.RollbackWake(ctx, id)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if state == "live_machine" {
					s.machines.Store(id, &vm.Machine{})
				}
				if state == "read_failure" {
					if err := s.reg.Close(); err != nil {
						t.Fatal(err)
					}
				}
				attemptErr := errors.Join(errors.New("launch failed"), vm.ErrLaunchExitUnconfirmed)
				// A rollback failure can also follow an ordinary launch error.
				if state == "wrong_row" || state == "read_failure" {
					attemptErr = errors.New("rollback failed")
				}
				var result error
				if kind == "adoption" {
					result = s.finishFailedAdoption(claim, attemptErr)
				} else {
					result = s.finishFailedWake(claim, attemptErr)
				}
				if !errors.Is(result, attemptErr) {
					t.Fatalf("lost attempt error: %v", result)
				}
				control, _, err := observer.readHandoff(ctx, id)
				want := handoffRunning
				if state == "confirmed" {
					want = handoffOffered
				}
				if err != nil || control == nil || control.Phase != want {
					t.Fatalf("handoff after %s rollback: %+v %v, want %s", state, control, err, want)
				}
				if state == "confirmed" {
					if _, err := observer.claimHandoff(ctx, id); err != nil {
						t.Fatalf("confirmed rollback blocked next claimant: %v", err)
					}
				}
			})
		}
	}
}
