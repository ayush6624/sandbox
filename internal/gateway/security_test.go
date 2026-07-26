package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
)

func secureTestGateway(t *testing.T) *Gateway {
	t.Helper()
	g := New("legacy", 20*time.Second, 0, 0)
	err := g.ConfigureSecurity(
		[]string{"client-token"}, "",
		[]string{"worker-token"}, "",
		management.Transport{Mode: management.TransportPrivateProxy},
	)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestConfigureSecurityRejectsSharedProductionCredential(t *testing.T) {
	g := New("legacy", 20*time.Second, 0, 0)
	err := g.ConfigureSecurity(
		[]string{"shared"}, "", []string{"shared"}, "",
		management.Transport{Mode: management.TransportPrivateProxy},
	)
	if err == nil {
		t.Fatal("shared client and worker credential accepted")
	}
}

func TestConfigureSecurityRejectsSecondaryRotationOverlap(t *testing.T) {
	g := New("legacy", 20*time.Second, 0, 0)
	err := g.ConfigureSecurity(
		[]string{"client-next", "shared-old"}, "",
		[]string{"worker-next", "shared-old"}, "",
		management.Transport{Mode: management.TransportPrivateProxy},
	)
	if err == nil {
		t.Fatal("secondary client/worker credential overlap accepted")
	}
}

func TestGatewayAuthSeparatesClientAndWorkerDomains(t *testing.T) {
	g := secureTestGateway(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := g.bearerAuth(next)

	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"client public", "/v1/sandboxes", "client-token", http.StatusNoContent},
		{"worker public denied", "/v1/sandboxes", "worker-token", http.StatusUnauthorized},
		{"worker internal", "/internal/v1/hosts:register", "worker-token", http.StatusNoContent},
		{"client internal denied", "/internal/v1/hosts:register", "client-token", http.StatusUnauthorized},
		{"query token denied", "/v1/sandboxes?access_token=client-token", "", http.StatusUnauthorized},
		{"websocket query token denied", "/v1/sandboxes/x/shell?access_token=client-token", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.name == "websocket query token denied" {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

func TestGatewayAuthFailsClosedAfterCredentialFilesOverlap(t *testing.T) {
	dir := t.TempDir()
	clientFile := dir + "/client"
	workerFile := dir + "/worker"
	if err := os.WriteFile(clientFile, []byte("client-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workerFile, []byte("worker-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := New("legacy", 20*time.Second, 0, 0)
	if err := g.ConfigureSecurity(
		nil, clientFile, nil, workerFile,
		management.Transport{Mode: management.TransportPrivateProxy},
	); err != nil {
		t.Fatal(err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := g.bearerAuth(next)

	nextWorkerFile := workerFile + ".next"
	if err := os.WriteFile(nextWorkerFile, []byte("client-token\nworker-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextWorkerFile, workerFile); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/sandboxes", "/internal/v1/hosts:register"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer client-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted overlapping credential: status=%d", path, w.Code)
		}
	}
}

func TestSecureGatewayRejectsPublicWorkerCallback(t *testing.T) {
	g := secureTestGateway(t)
	body := []byte(`{
		"host_id":"worker-1",
		"addr":"http://8.8.8.8:8080",
		"control_token":"callback",
		"slots_total":1,
		"sandbox_ids":[]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hosts:register", bytes.NewReader(body))
	w := httptest.NewRecorder()
	g.handleRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
