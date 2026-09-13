package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func TestCloneLaunchFailureKeepsResourcesUntilExitConfirmed(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed_exit", true: "confirmed_exit"}[confirmed], func(t *testing.T) {
			s := createRequestServer(t)
			t.Setenv("PATH", t.TempDir())
			ctx := context.Background()
			intent := memberIntent(t, createops.Member{ID: "clone-launch"})
			p, err := s.reg.BeginCreateProgress(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			intent.ProgressAttempt = p.Attempt
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(t.TempDir(), "disk")
			if err := os.WriteFile(disk, []byte("owned clone disk"), 0600); err != nil {
				t.Fatal(err)
			}
			sb, err := s.reg.CreateStarting(ctx, "clone", "", disk, nil, "", 0, 1, 128, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageVM); err != nil {
				t.Fatal(err)
			}
			launchErr := errors.New("injected clone launch failure")
			stopErr := errors.New("injected clone stop failure")
			var m *vm.Machine
			if !confirmed {
				m = &vm.Machine{}
				launchErr = errors.Join(launchErr, vm.ErrLaunchExitUnconfirmed)
			}
			stops := 0
			s.stopMachineFn = func(got *vm.Machine) error {
				stops++
				if got != m {
					t.Fatal("wrong launch stopped")
				}
				return stopErr
			}
			err = s.rollbackCloneLaunch(ctx, sb, m, launchErr)
			if !errors.Is(err, launchErr) {
				t.Fatalf("lost launch error: %v", err)
			}
			if !confirmed {
				if !errors.Is(err, stopErr) || stops != 1 {
					t.Fatalf("lost bounded stop: calls=%d err=%v", stops, err)
				}
				if got, ok := s.machines.Load(sb.ID); !ok || got != m {
					t.Fatal("lost unconfirmed launch")
				}
				if _, err := os.Stat(disk); err != nil {
					t.Fatalf("deleted owned disk: %v", err)
				}
				result, err := s.reg.CreateRequest(ctx, intent)
				if err != nil || result.Phase != "allocated" {
					t.Fatalf("terminalized live allocation: %+v %v", result, err)
				}
				p, err = s.reg.CreateProgress(ctx, intent.ID)
				if err != nil || p.Condition != "tearing_down" || p.Outcome != nil || p.Current.Stage != registry.CreateStageVM {
					t.Fatalf("live progress: %+v %v", p, err)
				}
				s.stopMachineFn = func(*vm.Machine) error { return nil }
				if err := s.destroy(ctx, sb.ID); err != nil {
					t.Fatal(err)
				}
			} else if stops != 0 {
				t.Fatalf("stopped confirmed dead launch %d times", stops)
			}
			if _, err := s.reg.Get(ctx, sb.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("retained confirmed dead allocation: %v", err)
			}
			if _, err := os.Stat(disk); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retained confirmed dead disk: %v", err)
			}
			result, err := s.reg.CreateRequest(ctx, intent)
			if err != nil || result.Phase != "failed" || result.Code != "vm_start_failed" {
				t.Fatalf("terminal result: %+v %v", result, err)
			}
		})
	}
}
