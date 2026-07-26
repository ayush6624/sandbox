package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopMachineForcesAfterShutdownTimeout(t *testing.T) {
	var forced atomic.Bool
	ops := machineStopOps{
		shutdown: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
		wait: func(ctx context.Context) error {
			if forced.Load() {
				return errors.New("expected signal exit")
			}
			<-ctx.Done()
			return ctx.Err()
		},
		force: func() error {
			forced.Store(true)
			return nil
		},
	}

	start := time.Now()
	if err := stopMachineWith(ops, 10*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !forced.Load() {
		t.Fatal("stuck shutdown was not force-stopped")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("bounded stop took %s", elapsed)
	}
}

func TestStopMachineForcesWhenGuestDoesNotExit(t *testing.T) {
	var forced atomic.Bool
	ops := machineStopOps{
		shutdown: func(context.Context) error { return nil },
		wait: func(ctx context.Context) error {
			if forced.Load() {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		},
		force: func() error {
			forced.Store(true)
			return nil
		},
	}

	if err := stopMachineWith(ops, time.Second, 10*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if !forced.Load() {
		t.Fatal("non-exiting guest was not force-stopped")
	}
}

func TestStopMachineReportsForceFailure(t *testing.T) {
	want := errors.New("force failed")
	ops := machineStopOps{
		shutdown: func(context.Context) error { return context.DeadlineExceeded },
		wait:     func(context.Context) error { return context.DeadlineExceeded },
		force:    func() error { return want },
	}
	if err := stopMachineWith(ops, time.Millisecond, time.Millisecond, time.Millisecond); !errors.Is(err, want) {
		t.Fatalf("stop error = %v, want %v", err, want)
	}
}
