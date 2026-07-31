package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/ayush6624/sandbox/internal/vm"
)

// watchMachine reaps host resources when a VMM exits without going through an
// expected destroy/hibernate path. Expected lifecycle operations hold the same
// wake lock and remove the machine from the map before this cleanup runs, so
// the watcher becomes a no-op instead of racing a second teardown.
func (s *Server) watchMachine(id string, m *vm.Machine, label string) {
	go func() {
		waitErr := vm.Wait(context.Background(), m)
		cleanupErr := s.cleanupExitedMachine(id, m)
		switch {
		case cleanupErr != nil:
			fmt.Fprintf(os.Stderr, "[%s] %s exited (%v); cleanup failed: %v\n", id, label, waitErr, cleanupErr)
		case waitErr != nil:
			fmt.Fprintf(os.Stderr, "[%s] %s exited unexpectedly: %v\n", id, label, waitErr)
		default:
			fmt.Fprintf(os.Stderr, "[%s] %s exited\n", id, label)
		}
	}()
}

func (s *Server) cleanupExitedMachine(id string, exited *vm.Machine) error {
	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()

	current, ok := s.machines.Load(id)
	if !ok || current != exited {
		return nil
	}
	s.machines.Delete(id)
	s.act.forget(id)
	s.diffBase.Delete(id)
	s.clearHibernationLineage(id)

	ctx := context.Background()
	sb, err := s.reg.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}
	if err := s.reg.MarkStopping(ctx, id); err != nil {
		return fmt.Errorf("mark stopping: %w", err)
	}

	ports, portsErr := s.reg.Ports(ctx, id)
	s.pf.CloseSandbox(id)
	for _, pm := range ports {
		s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
	}
	tapErr := s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	rootfsErr := s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	regErr := s.reg.Destroy(ctx, id)

	return errors.Join(portsErr, tapErr, rootfsErr, regErr)
}
