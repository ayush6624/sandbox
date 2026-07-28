package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func (s *Server) startWarmPool(ctx context.Context) {
	if s.cfg.WarmPoolSize <= 0 || s.golden.Load() == nil {
		return
	}
	s.warmOnce.Do(func() {
		go s.maintainWarmPool(ctx)
	})
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
		return registry.Sandbox{}, false
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claim warm sandbox: %v\n", err)
		return registry.Sandbox{}, false
	}
	s.act.touch(sb.ID)
	s.kickWarmPool()
	return sb, true
}

func (s *Server) maintainWarmPool(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		count, err := s.reg.WarmCount(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warm pool count: %v\n", err)
			if !waitWarmRetry(ctx, s.warmKick) {
				return
			}
			continue
		}
		if count >= s.cfg.WarmPoolSize {
			select {
			case <-ctx.Done():
				return
			case <-s.warmKick:
				continue
			}
		}

		if err := s.acquireCreate(ctx); err != nil {
			return
		}
		snap := s.golden.Load()
		if snap == nil {
			s.releaseCreate()
			return
		}
		_, err = s.createWarmFromSnapshot(ctx, *snap)
		s.releaseCreate()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warm pool replenish: %v\n", err)
			if !waitWarmRetry(ctx, s.warmKick) {
				return
			}
		}
	}
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
	c := s.bringUpClone(snap, "", nil, -1, true)
	stage.Unlock()
	if c.err != nil {
		return registry.Sandbox{}, c.err
	}
	if err := s.finishClone(ctx, c); err != nil {
		_ = s.destroy(context.Background(), c.sb.ID)
		return registry.Sandbox{}, err
	}
	fmt.Fprintf(os.Stderr, "[%s] warm sandbox ready from golden snapshot %s in %s\n",
		c.sb.ID, snap.ID, time.Since(started).Round(time.Millisecond))
	return c.sb, nil
}
