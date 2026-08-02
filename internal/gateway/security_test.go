package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/wsutil"
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

// edgeTestGateway is secureTestGateway plus the third (edge) credential domain.
func edgeTestGateway(t *testing.T) *Gateway {
	t.Helper()
	g := secureTestGateway(t)
	if err := g.ConfigureEdgeCredentials([]string{"edge-token"}, ""); err != nil {
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

// TestConfigureEdgeCredentialsRejectsOverlap keeps the third domain disjoint
// from the other two: an edge token equal to the client token would re-open the
// exact disclosure the domain exists to close, and one equal to the worker token
// would hand the edge fleet-control authority it never needs.
func TestConfigureEdgeCredentialsRejectsOverlap(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"edge equals client", "client-token"},
		{"edge equals worker", "worker-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := secureTestGateway(t)
			if err := g.ConfigureEdgeCredentials([]string{tt.token}, ""); err == nil {
				t.Fatal("overlapping edge credential accepted")
			}
			// A rejected configuration must not half-apply, or the gateway would
			// run with an edge domain nobody validated.
			if g.edgeCredentials != nil {
				t.Fatal("rejected edge credential was retained")
			}
		})
	}
}

// TestGatewayAuthEdgeAndControlDomains is the S1/S2 regression: the routes that
// hand out a worker control token, and the routes that can take the fleet down,
// must not be reachable with the credential end users hold as SANDBOX_API_KEY.
func TestGatewayAuthEdgeAndControlDomains(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		// (a) the client credential no longer buys a worker control token.
		{"client on route denied", "/route/sb-1", "client-token", http.StatusUnauthorized},
		{"client on raw-route denied", "/raw-route/20001", "client-token", http.StatusUnauthorized},
		// (b) the edge credential is what opens those two.
		{"edge on route", "/route/sb-1", "edge-token", http.StatusNoContent},
		{"edge on raw-route", "/raw-route/20001", "edge-token", http.StatusNoContent},
		// The edge is scoped to route lookup only — it is not an operator and
		// not a tenant.
		{"edge on client route denied", "/sandboxes", "edge-token", http.StatusUnauthorized},
		{"edge on worker route denied", "/internal/v1/hosts", "edge-token", http.StatusUnauthorized},
		{"worker on route denied", "/route/sb-1", "worker-token", http.StatusUnauthorized},
		// (c) legacy fleet-control aliases moved to the worker domain.
		{"client on worker-release denied", "/worker-release", "client-token", http.StatusUnauthorized},
		{"client on hosts denied", "/hosts", "client-token", http.StatusUnauthorized},
		{"client on drain denied", "/hosts/worker-1/drain", "client-token", http.StatusUnauthorized},
		{"worker on worker-release", "/worker-release", "worker-token", http.StatusNoContent},
		{"worker on hosts", "/hosts", "worker-token", http.StatusNoContent},
		{"worker on drain", "/hosts/worker-1/drain", "worker-token", http.StatusNoContent},
		// Allocating a raw public port stays a tenant operation: it returns no
		// worker credential, so moving it would break the SDK for no gain.
		{"client on raw-ports", "/sandboxes/sb-1/raw-ports", "client-token", http.StatusNoContent},
		{"worker on raw-ports denied", "/sandboxes/sb-1/raw-ports", "worker-token", http.StatusUnauthorized},
		// /metrics/hosts is federation, not the fleet inventory — the /hosts
		// classification must not swallow it.
		{"client on metrics hosts", "/metrics/hosts", "client-token", http.StatusNoContent},
	}
	g := edgeTestGateway(t)
	handler := g.bearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

// TestGatewayAuthEdgeFallbackWithoutEdgeCredential pins the compatibility path.
// The edge deployed today authenticates with the client credential, so a gateway
// with no edge credential configured MUST keep accepting it — otherwise the
// rollout that ships this change breaks public ingress fleet-wide. The
// disclosure is instead surfaced as a startup WARNING.
func TestGatewayAuthEdgeFallbackWithoutEdgeCredential(t *testing.T) {
	g := secureTestGateway(t)
	if g.edgeCredentials != nil {
		t.Fatal("edge credentials unexpectedly configured")
	}
	handler := g.bearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"client on route", "/route/sb-1", "client-token", http.StatusNoContent},
		{"client on raw-route", "/raw-route/20001", "client-token", http.StatusNoContent},
		{"bad token on route", "/route/sb-1", "nope", http.StatusUnauthorized},
		// The fallback is only for the edge domain; fleet control never loosens.
		{"client on hosts still denied", "/hosts", "client-token", http.StatusUnauthorized},
		{"worker on route denied", "/route/sb-1", "worker-token", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

// TestGatewayAuthAcceptsSubprotocolCredentialOnUpgrades pins the only
// browser-reachable credential channel for the shell WebSocket: browsers can't
// set headers and query credentials stay rejected, so the token rides in
// Sec-WebSocket-Protocol — but only on upgrades, and never onto worker routes.
func TestGatewayAuthAcceptsSubprotocolCredentialOnUpgrades(t *testing.T) {
	g := secureTestGateway(t)
	handler := g.bearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	bearer := func(tok string) string {
		return wsutil.SubprotocolBearerPrefix + base64.RawURLEncoding.EncodeToString([]byte(tok))
	}

	tests := []struct {
		name    string
		path    string
		token   string
		upgrade bool
		status  int
	}{
		{"client token on an upgrade", "/v1/sandboxes/x/shell", "client-token", true, http.StatusNoContent},
		{"bad token on an upgrade", "/v1/sandboxes/x/shell", "nope", true, http.StatusUnauthorized},
		{"worker token on a public upgrade denied", "/v1/sandboxes/x/shell", "worker-token", true, http.StatusUnauthorized},
		{"client token on a non-upgrade denied", "/v1/sandboxes", "client-token", false, http.StatusUnauthorized},
		{"worker route via subprotocol denied", "/internal/v1/hosts:register", "worker-token", true, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			req.Header.Add("Sec-WebSocket-Protocol", bearer(tt.token))
			req.Header.Add("Sec-WebSocket-Protocol", wsutil.SubprotocolShell)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

// TestHostProxyStripsSubprotocolCredential asserts the consumed credential
// never rides to the worker while the negotiable offer is preserved so the
// upgrade can still be completed.
func TestHostProxyStripsSubprotocolCredential(t *testing.T) {
	g := secureTestGateway(t)
	proxy := g.buildHostProxy("host-1", "10.0.0.5:8080", "worker-token")

	req := httptest.NewRequest(http.MethodGet, "http://gw/sandboxes/x/shell", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Add("Sec-WebSocket-Protocol",
		wsutil.SubprotocolBearerPrefix+base64.RawURLEncoding.EncodeToString([]byte("client-token")))
	req.Header.Add("Sec-WebSocket-Protocol", wsutil.SubprotocolShell)
	proxy.Director(req)

	if got := wsutil.BearerSubprotocol(req); got != "" {
		t.Fatalf("credential forwarded to the worker: %q", got)
	}
	if got := wsutil.NegotiatedSubprotocol(req); got != wsutil.SubprotocolShell {
		t.Fatalf("negotiable offer = %q, want %q", got, wsutil.SubprotocolShell)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer worker-token" {
		t.Fatalf("Authorization = %q, want the injected worker token", got)
	}

	// The guest agent never negotiates, so the proxy must finish it.
	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: make(http.Header), Request: req}
	if err := proxy.ModifyResponse(resp); err != nil {
		t.Fatalf("ModifyResponse: %v", err)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != wsutil.SubprotocolShell {
		t.Fatalf("echoed subprotocol = %q, want %q", got, wsutil.SubprotocolShell)
	}
}

func TestHostProxyDecoratesRawPortListsWithPublicHost(t *testing.T) {
	g := secureTestGateway(t)
	g.raw = &rawAllocator{cfg: RawConfig{PublicHost: "tcp.sandboxes.example.com"}}
	proxy := g.buildHostProxy("host-1", "10.0.0.5:8080", "worker-token")

	req := httptest.NewRequest(http.MethodGet, "http://gw/sandboxes/x/ports", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Request:    req,
		Body: io.NopCloser(bytes.NewBufferString(
			`[{"guest_port":22,"public_port":20000,"mode":"raw"},` +
				`{"guest_port":3000,"url":"https://3000-x.example.com","mode":"url"}]`,
		)),
	}
	if err := proxy.ModifyResponse(resp); err != nil {
		t.Fatalf("ModifyResponse: %v", err)
	}
	var ports []registry.PortMapping
	if err := json.NewDecoder(resp.Body).Decode(&ports); err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 || ports[0].PublicHost != "tcp.sandboxes.example.com" {
		t.Fatalf("raw mapping was not decorated: %+v", ports)
	}
	if ports[1].PublicHost != "" {
		t.Fatalf("non-raw mapping gained a public host: %+v", ports[1])
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
