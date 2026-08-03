package client

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestNewHTTPPreservesEncryptedEndpoint(t *testing.T) {
	c := NewHTTP("https://worker.example:8443/", "token")
	if c.baseURL != "https://worker.example:8443" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
	if c.wsURL != "wss://worker.example:8443" {
		t.Fatalf("wsURL = %q", c.wsURL)
	}
}

func TestNewHTTPDefaultsLegacyAddressToHTTP(t *testing.T) {
	c := NewHTTP("127.0.0.1:8080", "token")
	if c.baseURL != "http://127.0.0.1:8080" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
}

func TestDialSandboxPortUsesAuthenticatedV1WebSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sandboxes/abc/connect/22" {
			t.Errorf("tunnel request = %s %s", r.Method, r.URL.Path)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tenant-key" {
			t.Errorf("missing tenant authorization")
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer ws.CloseNow()
		conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		defer conn.Close()
		_, _ = io.WriteString(conn, "SSH-2.0-test\r\n")
	}))
	defer srv.Close()
	c := NewHTTP(srv.URL, "tenant-key")
	conn, err := c.DialSandboxPort(context.Background(), "abc", 22)
	if err != nil {
		t.Fatalf("DialSandboxPort: %v", err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "SSH-2.0-test\r\n" {
		t.Fatalf("first guest bytes = %q, %v", line, err)
	}
}

func TestPrepareSSHTunnelUsesOnlyCLIKeyEndpoint(t *testing.T) {
	requests := 0
	c := &Client{
		baseURL: "https://api.example.com",
		http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.Method != http.MethodPut || req.URL.Path != "/v1/sandboxes/abc/ssh-access" {
				t.Fatalf("request = %s %s", req.Method, req.URL.Path)
			}
			if req.Header.Get("Idempotency-Key") == "" {
				t.Fatal("missing Idempotency-Key")
			}
			return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	if err := c.PrepareSSHTunnel(context.Background(), "abc", "ssh-ed25519 AAAA"); err != nil {
		t.Fatalf("PrepareSSHTunnel: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want only the SSH access mutation", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
