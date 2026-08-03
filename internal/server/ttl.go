package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// handleSetTimeout updates a sandbox's auto-destroy deadline:
// timeout_sec > 0 sets expires_at = now + timeout_sec; 0 clears it.
func (s *Server) handleSetTimeout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		TimeoutSec int `json:"timeout_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if err := s.reg.SetExpiry(r.Context(), id, expiresAt); err != nil {
		httpError(w, 404, err)
		return
	}
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, s.effectiveResources(sb))
}

// reapExpired periodically destroys sandboxes whose TTL has passed.
// Runs until ctx (the server lifetime) is cancelled.
func (s *Server) reapExpired(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			expired, err := s.reg.Expired(ctx, now)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reaper: list expired: %v\n", err)
				continue
			}
			for _, sb := range expired {
				fmt.Fprintf(os.Stderr, "reaper: destroying expired sandbox %s (expired %s)\n",
					sb.ID, sb.ExpiresAt.Format(time.RFC3339))
				if err := s.destroyExpired(context.Background(), sb.ID, now); err != nil {
					fmt.Fprintf(os.Stderr, "reaper: destroy %s: %v\n", sb.ID, err)
				}
			}
			expiredSnapshots, err := s.reg.ExpiredSnapshots(ctx, now)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reaper: list expired snapshots: %v\n", err)
				continue
			}
			for _, snap := range expiredSnapshots {
				fmt.Fprintf(os.Stderr, "reaper: deleting expired snapshot %s (expired %s)\n",
					snap.ID, snap.ExpiresAt.Format(time.RFC3339))
				if err := s.deleteExpiredSnapshot(context.Background(), snap.ID, now); err != nil {
					// An in-use snapshot remains valid and is retried after its
					// dependent resources are deleted.
					fmt.Fprintf(os.Stderr, "reaper: delete snapshot %s: %v\n", snap.ID, err)
				}
			}
		}
	}
}

// destroyExpired revalidates the deadline under the lifecycle lock. A timeout
// extension that lands after Expired's scan must win over the stale reaper row.
func (s *Server) destroyExpired(ctx context.Context, id string, cutoff time.Time) error {
	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return err
	}
	if sb.ExpiresAt == nil || !sb.ExpiresAt.Before(cutoff) {
		return nil
	}
	// Attribute the close to the TTL before the generic destroy path claims it.
	// The VM is still live, so this also takes the final CPU sample; the close
	// inside destroyLocked is then a no-op. Worth the extra call: "expire" is
	// the answer to a customer asking why a sandbox vanished, and a bare
	// "destroy" would imply someone deleted it.
	s.meterStop(ctx, id, registry.EndExpire)
	return s.destroyLocked(ctx, id)
}
