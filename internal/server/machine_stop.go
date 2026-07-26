package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ayush6624/sandbox/internal/vm"
)

const (
	guestShutdownGrace = 2 * time.Second
	guestExitGrace     = 2 * time.Second
	forcedExitGrace    = 5 * time.Second
)

type machineStopOps struct {
	shutdown func(context.Context) error
	wait     func(context.Context) error
	force    func() error
}

// stopMachineBounded gives the guest a short clean-shutdown window, then
// guarantees that a stuck ACPI path cannot consume the SDK's 30-second request
// timeout. Disposable sandboxes favor bounded teardown over an extended guest
// grace period.
func stopMachineBounded(m *vm.Machine) error {
	return stopMachineWith(machineStopOps{
		shutdown: func(ctx context.Context) error { return vm.ShutdownGuest(ctx, m) },
		wait:     func(ctx context.Context) error { return vm.Wait(ctx, m) },
		force:    func() error { return vm.StopForce(m) },
	}, guestShutdownGrace, guestExitGrace, forcedExitGrace)
}

func stopMachineWith(ops machineStopOps, shutdownGrace, exitGrace, forceGrace time.Duration) error {
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- ops.shutdown(shutdownCtx) }()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdownDone:
	case <-shutdownCtx.Done():
		shutdownErr = shutdownCtx.Err()
	}
	cancelShutdown()

	// A successful shutdown request still needs an independently bounded exit
	// wait. If the shutdown call itself timed out, force immediately.
	if shutdownErr == nil {
		if exited, _ := waitMachine(ops.wait, exitGrace); exited {
			return nil
		}
	}

	if err := ops.force(); err != nil {
		return fmt.Errorf("force stop: %w", err)
	}
	if exited, err := waitMachine(ops.wait, forceGrace); !exited {
		return fmt.Errorf("wait after force stop: %w", err)
	}
	return nil
}

// vm.Wait returns the process's exit error when it has exited. Only a context
// error means the process is still running.
func waitMachine(wait func(context.Context) error, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false, err
	}
	return true, err
}
