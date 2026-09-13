package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/registry"
)

func inlineTerminalProgress(t *testing.T, s *Server, store createops.Store, target registry.CreateProgressTarget) (registry.CreateProgress, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, intent := deliveryMember(t, s, store, "retained", target)
	p, err := s.reg.BeginCreateProgress(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = p.Attempt
	if err := s.reg.FailCreateRequest(ctx, intent, 404, "source_not_found", "The requested source was not found."); err != nil {
		t.Fatal(err)
	}
	p, err = s.reg.CreateProgress(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", s.reg.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE create_progress SET snapshot=? WHERE id=?`, body, p.ID); err != nil {
		t.Fatal(err)
	}
	return p, db
}

func TestProgressRetentionRunsWithEmptyDeliveryPage(t *testing.T) {
	ctx := context.Background()
	s := createRequestServer(t)
	store, _ := deliveryStore(t)
	p, db := inlineTerminalProgress(t, s, store, registry.CreateProgressLocal)
	if err := s.deliverCreateProgress(ctx, store, &http.Client{}, p); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(ctx, "", 10); err != nil || len(pending) != 0 {
		t.Fatalf("expected empty delivery page: %+v, %v", pending, err)
	}
	before, err := store.Get(ctx, p.Owner.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	done := make(chan struct{})
	go func() { s.runCreateProgressDelivery(deliveryCtx, store); close(done) }()
	defer func() { cancel(); <-done }()
	for {
		var inline int
		if err := db.QueryRowContext(deliveryCtx, `SELECT COUNT(*) FROM create_progress WHERE id=? AND json_type(snapshot,'$.Outcome')='object'`, p.ID).Scan(&inline); err != nil {
			t.Fatal(err)
		}
		if inline == 0 {
			break
		}
		select {
		case <-deliveryCtx.Done():
			t.Fatal("empty delivery pages prevented old progress conversion")
		case <-time.After(10 * time.Millisecond):
		}
	}
	got, err := s.reg.CreateProgress(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("conversion changed progress: %+v, %v", got, err)
	}
	after, err := store.Get(ctx, p.Owner.OperationID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("conversion changed coordinator history: %+v, %v", after, err)
	}
}

func TestCompactTerminalProgressRetriesLostGatewayAcknowledgement(t *testing.T) {
	ctx := context.Background()
	s := createRequestServer(t)
	store, path := deliveryStore(t)
	p, _ := inlineTerminalProgress(t, s, store, registry.CreateProgressGateway)
	var err error
	s.gatewayCredentials, err = management.NewCredentials([]string{"retention-worker-key"}, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(target createops.Store, loseAck bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer retention-worker-key" || r.URL.Path != "/internal/v1/create-progress" {
				t.Error("wrong delivery credentials or path")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var update createops.ProgressUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !reflect.DeepEqual(update.Progress, p) {
				t.Error("wire observation changed during compaction")
			}
			sequence, err := target.IngestProgress(r.Context(), update.Worker, update.Progress)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusConflict)
				return
			}
			if loseAck {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(createops.ProgressAcknowledgement{Sequence: sequence})
		})
	}
	first := httptest.NewServer(handler(store, true))
	s.cfg.GatewayURL = first.URL
	_, err = s.deliverCreateProgressPage(ctx, store, first.Client(), "")
	first.Close()
	if err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	before, err := store.Get(ctx, p.Owner.OperationID)
	if err != nil || before.Status != "failed" || before.CompletedAt == nil {
		t.Fatalf("terminal ingestion was not durable: %+v, %v", before, err)
	}
	if _, err := s.reg.CompactCreateProgress(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(ctx, "", 10); err != nil || len(pending) != 1 || !reflect.DeepEqual(pending[0], p) {
		t.Fatalf("conversion lost pending terminal observation: %+v, %v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second := httptest.NewServer(handler(reopened, false))
	defer second.Close()
	s.cfg.GatewayURL = second.URL
	if _, err := s.deliverCreateProgressPage(ctx, reopened, second.Client(), ""); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(ctx, "", 10); err != nil || len(pending) != 0 {
		t.Fatalf("matching replay was not acknowledged: %+v, %v", pending, err)
	}
	after, err := reopened.Get(ctx, p.Owner.OperationID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("same-sequence replay changed public history: %+v, %v", after, err)
	}
}
