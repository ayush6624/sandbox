package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
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
	if err := s.reg.SetExpiry(r.Context(), id, expiresAt); err != nil {
		httpError(w, 404, err)
		return
	}
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, sb)
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
				if err := s.destroy(context.Background(), sb.ID); err != nil {
					fmt.Fprintf(os.Stderr, "reaper: destroy %s: %v\n", sb.ID, err)
				}
			}
		}
	}
}
