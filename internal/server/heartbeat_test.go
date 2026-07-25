package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
)

func TestWarmCompletionSendsImmediateHeartbeat(t *testing.T) {
	heartbeats := make(chan cluster.Heartbeat, 3)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb cluster.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Errorf("decode heartbeat: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		heartbeats <- hb
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()

	s, _ := capacityTestServer(t)
	s.cfg.GatewayURL = gateway.URL
	s.cfg.AdvertiseAddr = "worker:8080"
	s.cfg.HostID = "worker-1"
	s.cfg.WorkerRelease = "release-2"
	s.warmed = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.heartbeat(ctx)

	select {
	case hb := <-heartbeats:
		if hb.Release != "release-2" {
			t.Fatalf("release = %q, want release-2", hb.Release)
		}
		if hb.SlotsFree == nil || *hb.SlotsFree != 0 {
			t.Fatalf("initial slots_free = %v, want 0 while warming", hb.SlotsFree)
		}
	case <-time.After(time.Second):
		t.Fatal("initial heartbeat not received")
	}

	start := time.Now()
	close(s.warmed)
	select {
	case hb := <-heartbeats:
		if hb.SlotsFree == nil || *hb.SlotsFree != 3 {
			t.Fatalf("warm slots_free = %v, want 3", hb.SlotsFree)
		}
		if elapsed := time.Since(start); elapsed >= heartbeatInterval {
			t.Fatalf("warm heartbeat waited for periodic tick: %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("warm completion did not send an immediate heartbeat")
	}
}
