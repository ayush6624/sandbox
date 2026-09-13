package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func awaitHandoffShutdownAdmission(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.handoffShutdown.mu.Lock()
		draining := s.handoffShutdown.draining
		s.handoffShutdown.mu.Unlock()
		if draining {
			return
		}
		select {
		case <-deadline:
			t.Fatal("shutdown admission did not close")
		case <-ticker.C:
		}
	}
}

func TestHandoffShutdownWaitsForAdmittedReleaseAndBackupWhileServingPeer(t *testing.T) {
	store := newUploadTestStore(t)
	s := handoffTestServer(t, store, "source")
	entered, commit := make(chan struct{}), make(chan struct{})
	jobs := make(chan registry.HibernationHandoff, 1)
	handler := s.handoffShutdownHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/v1/hibernations/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		close(entered)
		<-commit
		jobs <- seedJournaledHandoff(t, s, uuid.NewString())
		w.WriteHeader(http.StatusNoContent)
	}))
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/sandboxes/id/release", nil))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.waitHandoffShutdown(ctx) }()
	awaitHandoffShutdownAdmission(t, s)
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/sandboxes/new/release", nil))
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatalf("new release accepted during drain: %d", denied.Code)
	}
	select {
	case err := <-drained:
		t.Fatalf("drain missed admitted release before journal commit: %v", err)
	default:
	}
	close(commit)
	job := <-jobs
	<-requestDone
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		peer := httptest.NewRecorder()
		handler.ServeHTTP(peer, httptest.NewRequest(method, "/internal/v1/hibernations/"+job.Generation, nil))
		if peer.Code != http.StatusNoContent {
			t.Fatalf("peer %s unavailable during drain: %d", method, peer.Code)
		}
	}
	select {
	case err := <-drained:
		t.Fatalf("drain stopped before backup completed: %v", err)
	default:
	}
	if err := s.reg.MarkHibernationHandoffPublished(context.Background(), job.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.WithHibernationHandoff(context.Background(), job.Generation, func(a *registry.HandoffAttempt) error { return a.CompleteBackup(context.Background()) }); err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	retained, err := s.reg.GetHibernationHandoff(context.Background(), job.Generation)
	if err != nil || retained.CacheReady {
		t.Fatalf("drain mutated target cache acknowledgment: %+v %v", retained, err)
	}
}

func TestHandoffShutdownDeadlinePreservesPendingJournal(t *testing.T) {
	store := newUploadTestStore(t)
	s := handoffTestServer(t, store, "source")
	job := seedJournaledHandoff(t, s, uuid.NewString())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := s.waitHandoffShutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending backup did not bound shutdown: %v", err)
	}
	retained, err := s.reg.GetHibernationHandoff(context.Background(), job.Generation)
	if err != nil || retained.BackupComplete || retained.Published {
		t.Fatalf("deadline changed pending journal: %+v %v", retained, err)
	}
}
