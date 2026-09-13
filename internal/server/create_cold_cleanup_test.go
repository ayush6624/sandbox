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

func TestColdCreateFailureRetainsUnstoppedVM(t *testing.T) {
	for _, stage := range []string{"start", "pid", "finish_start"} {
		t.Run(stage, func(t *testing.T) {
			s := createRequestServer(t)
			t.Setenv("PATH", t.TempDir())
			ctx := context.Background()
			intent := memberIntent(t, createops.Member{ID: "cold-failure"})
			p, err := s.reg.BeginCreateProgress(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			intent.ProgressAttempt = p.Attempt
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(t.TempDir(), "disk")
			if err := os.WriteFile(disk, []byte("retained disk"), 0600); err != nil {
				t.Fatal(err)
			}
			sb, err := s.reg.CreateStarting(ctx, "cold", "", disk, nil, "", 0, 1, 128, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageVM); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected launch failure")
			start := func(context.Context, *vm.Machine) error { return nil }
			pid := func(*vm.Machine) (int, error) { return 123, nil }
			switch stage {
			case "start":
				start = func(context.Context, *vm.Machine) error { return errors.Join(injected, vm.ErrLaunchExitUnconfirmed) }
			case "pid":
				pid = func(*vm.Machine) (int, error) { return 0, injected }
			case "finish_start":
				ageLedger(t, s.reg.Path(), `CREATE TRIGGER reject_finish_start BEFORE UPDATE OF pid ON sandboxes WHEN NEW.pid > 0 BEGIN SELECT RAISE(ABORT, 'injected publication failure'); END`)
			}
			m := &vm.Machine{}
			stopErr := errors.New("injected stop failure")
			stops := 0
			s.stopMachineFn = func(got *vm.Machine) error {
				stops++
				if got != m {
					t.Fatal("stopped a different machine")
				}
				return stopErr
			}
			if _, err := s.startColdMachine(ctx, sb, m, vm.RuntimeConfig{}, start, pid); !errors.Is(err, stopErr) {
				t.Fatalf("lost cleanup failure: %v", err)
			}
			if stops != 1 {
				t.Fatalf("bounded stop calls = %d, want 1", stops)
			}
			if got, ok := s.machines.Load(sb.ID); !ok || got != m {
				t.Fatal("lost unstopped machine")
			}
			if _, err := os.Stat(disk); err != nil {
				t.Fatalf("lost unstopped machine disk: %v", err)
			}
			if _, err := s.reg.Get(ctx, sb.ID); err != nil {
				t.Fatal(err)
			}
			result, err := s.reg.CreateRequest(ctx, intent)
			if err != nil || result.Phase != "allocated" {
				t.Fatalf("terminalized unstopped VM: %+v %v", result, err)
			}
			p, err = s.reg.CreateProgress(ctx, intent.ID)
			if err != nil || p.Outcome != nil || p.Condition != "tearing_down" {
				t.Fatalf("progress: %+v %v", p, err)
			}
			s.stopMachineFn = func(*vm.Machine) error { return nil }
			if err := s.destroy(ctx, sb.ID); err != nil {
				t.Fatal(err)
			}
			result, err = s.reg.CreateRequest(ctx, intent)
			if err != nil || result.Phase != "failed" {
				t.Fatalf("confirmed teardown: %+v %v", result, err)
			}
		})
	}
}

func TestColdCreateStartOutcome(t *testing.T) {
	for _, failed := range []bool{true, false} {
		name := "started"
		if failed {
			name = "confirmed_failure"
		}
		t.Run(name, func(t *testing.T) {
			s := createRequestServer(t)
			t.Setenv("PATH", t.TempDir())
			ctx := context.Background()
			intent := memberIntent(t, createops.Member{ID: "cold-outcome"})
			p, err := s.reg.BeginCreateProgress(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			intent.ProgressAttempt = p.Attempt
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(t.TempDir(), "disk")
			if err := os.WriteFile(disk, []byte("disk"), 0600); err != nil {
				t.Fatal(err)
			}
			sb, err := s.reg.CreateStarting(ctx, "cold", "", disk, nil, "", 0, 1, 128, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageVM); err != nil {
				t.Fatal(err)
			}
			m := &vm.Machine{}
			injected := errors.New("confirmed failed start")
			start := func(context.Context, *vm.Machine) error {
				if failed {
					return injected
				}
				return nil
			}
			pid := func(*vm.Machine) (int, error) {
				if failed {
					t.Fatal("PID called after failed start")
				}
				return 123, nil
			}
			s.stopMachineFn = func(*vm.Machine) error { t.Fatal("unexpected stop"); return nil }
			gotPID, err := s.startColdMachine(ctx, sb, m, vm.RuntimeConfig{VMID: "vm", SocketPath: "/tmp/cold.sock"}, start, pid)
			if failed {
				if !errors.Is(err, injected) {
					t.Fatalf("start error: %v", err)
				}
				if _, ok := s.machines.Load(sb.ID); ok {
					t.Fatal("retained confirmed exited machine")
				}
				if _, err := os.Stat(disk); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disk was not removed: %v", err)
				}
				if _, err := s.reg.Get(ctx, sb.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("retained failed sandbox: %v", err)
				}
			} else {
				if err != nil || gotPID != 123 {
					t.Fatalf("start: %d %v", gotPID, err)
				}
				if got, ok := s.machines.Load(sb.ID); !ok || got != m {
					t.Fatal("lost started machine")
				}
				current, err := s.reg.Get(ctx, sb.ID)
				if err != nil || current.PID != 123 || current.VMID != "vm" || current.SocketPath != "/tmp/cold.sock" || current.Status != registry.StatusStarting {
					t.Fatalf("published start: %+v %v", current, err)
				}
			}
			result, err := s.reg.CreateRequest(ctx, intent)
			expected := "allocated"
			if failed {
				expected = "failed"
			}
			if err != nil || result.Phase != expected {
				t.Fatalf("ledger: %+v %v", result, err)
			}
			p, err = s.reg.CreateProgress(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failed {
				if p.Condition != "failed" || p.Outcome == nil {
					t.Fatalf("confirmed failure progress: %+v", p)
				}
			} else if p.Condition != "active" || p.Outcome != nil || p.Current.Stage != registry.CreateStageVM {
				t.Fatalf("start published premature ready: %+v", p)
			}
		})
	}
}
