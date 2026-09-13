package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

const maxPendingHandoffs = registry.MaxPendingHibernationHandoffs

func (s *Server) canHandoff() bool {
	if s.blob == nil || !s.cfg.UFFDChunkGCS || s.workerCredentials == nil {
		return false
	}
	_, err := s.privateAdvertiseURL()
	return err == nil
}

func (s *Server) handleHandoffRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	peer, err := s.releaseForHandoff(r.Context(), id)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		httpError(w, status, err)
		return
	}
	w.Header().Set("X-Sandbox-Hibernation-Generation", peer.Generation)
	if r.URL.Query().Get("durability") == "required" {
		if err := s.awaitHandoffBackup(r.Context(), id, peer.Generation); err != nil {
			httpError(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Source files and the upload journal survive request cancellation and removal
// of the serving row. Only the small ownership offer is published synchronously.
func (s *Server) releaseForHandoff(ctx context.Context, id string) (*hibPeerRef, error) {
	mu := s.wakeLock(id)
	mu.Lock()
	sb, err := s.reg.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		mu.Unlock()
		return s.replayHandoffRelease(ctx, id)
	}
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	jobs, err := s.reg.ListHibernationHandoffs(ctx)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if len(jobs) >= maxPendingHandoffs {
		mu.Unlock()
		return nil, fmt.Errorf("pending handoff limit reached")
	}
	if sb.Status == registry.StatusHibernated {
		s.cancelHibernationUpload(id)
		claim, err := s.claimLocalHandoff(ctx, id)
		if err == nil && claim != nil {
			err = s.authorizeHandoffRun(ctx, claim)
		}
		if err != nil {
			mu.Unlock()
			return nil, err
		}
	}
	err = s.reserveLocalHandoff(ctx, id)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	if sb.Status == registry.StatusRunning {
		if err := s.hibernateWithMode(ctx, id, hibernateMode{strictDurability: true, handoff: true}); err != nil {
			return nil, fmt.Errorf("freeze handoff: %w", err)
		}
	}
	mu = s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()
	sb, err = s.reg.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sb.Status != registry.StatusHibernated {
		return nil, fmt.Errorf("sandbox %s is %s, cannot hand off", id, sb.Status)
	}
	s.cancelHibernationUpload(id)
	desc, err := s.prepareLocalHandoff(ctx, sb)
	if err != nil {
		return nil, err
	}
	job, err := s.prepareHandoffOffer(ctx, id, desc)
	if err != nil {
		_ = os.RemoveAll(filepath.Join(s.hibPeerDir(), desc.Ref.Generation))
		return nil, err
	}
	if err := s.reg.CommitHibernationHandoff(ctx, job); err != nil {
		_ = os.RemoveAll(filepath.Join(s.hibPeerDir(), desc.Ref.Generation))
		return nil, err
	}
	s.pf.CloseSandbox(id)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	s.act.forget(id)
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		return nil, fmt.Errorf("handoff retained; offer publication pending: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[%s] released peer generation %s; cloud backup pending\n", id, desc.Ref.Generation)
	return &desc.Ref, nil
}

func (s *Server) replayHandoffRelease(ctx context.Context, id string) (*hibPeerRef, error) {
	control, revision, err := s.readHandoff(ctx, id)
	if err != nil {
		return nil, err
	}
	jobs, err := s.reg.ListHibernationHandoffs(ctx)
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if job.SandboxID != id {
			continue
		}
		if control != nil && control.Generation != job.Generation && (job.Published || job.ExpectedRevision != revision) {
			continue
		}
		if err := s.publishHandoffOffer(ctx, job); err != nil {
			return nil, err
		}
		control, _, err = s.readHandoff(ctx, id)
		if err != nil {
			return nil, err
		}
		break
	}
	if control == nil || control.SourceHostID != s.hostID() || control.Descriptor == nil {
		return nil, sql.ErrNoRows
	}
	return &control.Descriptor.Ref, nil
}

func (s *Server) awaitHandoffBackup(ctx context.Context, id, generation string) error {
	ctx, cancel := context.WithTimeout(ctx, releaseDurableWait)
	defer cancel()
	control, _, err := s.readHandoff(ctx, id)
	if err != nil {
		return err
	}
	if control != nil && control.Generation != generation {
		return ErrOwnerContended
	}
	object := handoffRecordObj(id, generation)
	var expected []byte
	if control != nil && control.Descriptor != nil {
		object = control.Descriptor.artifactObject("record.json")
		expected, _ = json.Marshal(control.Descriptor)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := s.blob.GetBytes(ctx, object); err == nil && len(data) > 0 {
			var durable retainedHibernation
			if err := json.Unmarshal(data, &durable); err != nil {
				return err
			}
			if err := durable.validateStorage(); err != nil {
				return err
			}
			if durable.Record.ID != id || durable.Record.Generation != generation || durable.Ref.Generation != generation {
				return fmt.Errorf("handoff backup identity mismatch")
			}
			got, _ := json.Marshal(durable)
			if expected != nil && !bytes.Equal(expected, got) {
				return fmt.Errorf("handoff backup descriptor mismatch")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("handoff backup not yet complete: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
