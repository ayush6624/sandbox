package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func (s *Server) startWarmPool(ctx context.Context) {
	if s.cfg.WarmPoolSize <= 0 {
		return
	}
	if s.golden.Load() == nil {
		s.settleReadyPool()
		return
	}
	s.warmOnce.Do(func() {
		go func() {
			if !s.waitForWarmPoolWindow(ctx) {
				return
			}
			// A broken pool must not make the host permanently unplaceable:
			// after a bounded attempt window, expose normal secure clone
			// capacity while the maintainer keeps retrying.
			timeout := time.AfterFunc(30*time.Second, s.settleReadyPool)
			defer timeout.Stop()
			s.maintainWarmPool(ctx)
		}()
	})
}

func (s *Server) settleReadyPool() {
	s.readyPoolSettledOnce.Do(func() { close(s.readyPoolSettled) })
}

// waitForWarmPoolWindow avoids starting nested VMs on refill instances while
// the MIG is trying to suspend them. PlacementDelay is intentionally later
// than the standby initial delay; a genuinely active worker fills the pool
// when it becomes placement-eligible, while a resumed standby's boottime age
// already clears the gate.
func (s *Server) waitForWarmPoolWindow(ctx context.Context) bool {
	for s.cfg.PlacementDelay > 0 {
		age, err := s.bootAge()
		if err == nil && age >= s.cfg.PlacementDelay {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return ctx.Err() == nil
}

func (s *Server) kickWarmPool() {
	select {
	case s.warmKick <- struct{}{}:
	default:
	}
}

// claimWarm promotes a fully initialized hidden VM into the routed inventory.
// No security setup is skipped: the replenisher already performed the jailed
// launch, network re-identification, clock sync, and SSH host-key rotation.
func (s *Server) claimWarm(ctx context.Context, name string, expiresAt *time.Time, idleTimeout int) (registry.Sandbox, bool) {
	sb, err := s.reg.ClaimWarm(ctx, name, expiresAt, idleTimeout)
	if errors.Is(err, sql.ErrNoRows) {
		s.met.warmMisses.Add(1)
		return registry.Sandbox{}, false
	}
	if err != nil {
		s.met.warmMisses.Add(1)
		fmt.Fprintf(os.Stderr, "claim warm sandbox: %v\n", err)
		return registry.Sandbox{}, false
	}
	s.met.warmClaims.Add(1)
	s.act.touch(sb.ID)
	// A ready VM has been running at our expense since the pool built it.
	// Billing starts at the claim — ClaimWarm also resets created_at to this
	// moment, so the row and the ledger agree on when the customer got it.
	s.meterStart(ctx, sb)
	s.kickWarmPool()
	return sb, true
}

func (s *Server) maintainWarmPool(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		ready, preparing, err := s.reg.WarmInventory(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warm pool count: %v\n", err)
			if !waitWarmRetry(ctx, s.warmKick) {
				return
			}
			continue
		}
		if ready+preparing >= s.cfg.WarmPoolSize {
			if ready >= s.cfg.WarmPoolSize {
				s.settleReadyPool()
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.warmKick:
				timer.Stop()
				continue
			case <-timer.C:
				continue
			}
		}

		// Fill the deficit concurrently. Each build first creates a
		// StatusPreparing row, so requests cannot claim it until every
		// readiness/security gate completes and MarkWarmReady promotes it.
		missing := s.cfg.WarmPoolSize - ready - preparing
		var wg sync.WaitGroup
		errs := make(chan error, missing)
		for range missing {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.buildWarmOne(ctx); err != nil {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		failed := false
		for err := range errs {
			failed = true
			s.met.warmFailures.Add(1)
			fmt.Fprintf(os.Stderr, "warm pool replenish: %v\n", err)
		}
		if failed && !waitWarmRetry(ctx, s.warmKick) {
			return
		}
	}
}

func (s *Server) buildWarmOne(ctx context.Context) error {
	if err := s.acquireCreate(ctx); err != nil {
		return err
	}
	defer s.releaseCreate()
	snap := s.golden.Load()
	if snap == nil {
		return errors.New("golden snapshot unavailable")
	}
	_, err := s.createWarmFromSnapshot(ctx, *snap)
	return err
}

func waitWarmRetry(ctx context.Context, kick <-chan struct{}) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-kick:
	case <-timer.C:
	}
	return true
}

func (s *Server) createWarmFromSnapshot(ctx context.Context, snap registry.Snapshot) (registry.Sandbox, error) {
	stage := s.snapshotStageLock(snap.SourceRootfsPath)
	stage.Lock()
	if err := s.stageSnapshotRootfs(snap); err != nil {
		stage.Unlock()
		return registry.Sandbox{}, fmt.Errorf("stage snapshot rootfs: %w", err)
	}
	started := time.Now()
	c := s.bringUpClone(ctx, snap, "", nil, -1, true)
	stage.Unlock()
	if c.err != nil {
		return registry.Sandbox{}, c.err
	}
	if err := s.finishClone(ctx, c); err != nil {
		_ = s.destroy(context.Background(), c.sb.ID)
		return registry.Sandbox{}, err
	}
	if err := s.reg.MarkWarmReady(ctx, c.sb.ID); err != nil {
		_ = s.destroy(context.Background(), c.sb.ID)
		return registry.Sandbox{}, fmt.Errorf("mark warm ready: %w", err)
	}
	c.sb.Status = registry.StatusWarming
	fmt.Fprintf(os.Stderr, "[%s] warm sandbox ready from golden snapshot %s in %s\n",
		c.sb.ID, snap.ID, time.Since(started).Round(time.Millisecond))
	return c.sb, nil
}
