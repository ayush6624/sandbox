package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ayush6624/sandbox/internal/httpapi"
)

// The edge always reaches handleConnectPort through the same instrumented
// handler chain the real listeners build (httpapi.Middleware wraps the writer
// in a statusWriter that exposes only Unwrap). A handler that type-asserts
// http.Hijacker directly fails there while passing a test that calls it with a
// bare hijacking writer, so this test drives the tunnel over a real socket
// through the middleware.
func TestConnectTunnelsThroughInstrumentedMiddleware(t *testing.T) {
	s := connectTestServer(t)
	if _, err := s.reg.AddURLPort(context.Background(), "sandbox-id", 3000); err != nil {
		t.Fatal(err)
	}
	workerBackend, guest := net.Pipe()
	s.pf.dial = func(context.Context, string, int) (net.Conn, error) {
		return workerBackend, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("CONNECT /sandboxes/{id}/connect/{port}", s.handleConnectPort)
	srv := httptest.NewServer(httpapi.Middleware(mux))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Byte-for-byte the request services/sandbox-edge's connectWorker writes.
	if _, err := io.WriteString(conn, "CONNECT /sandboxes/sandbox-id/connect/3000 HTTP/1.1\r\n"+
		"Host: worker\r\nAuthorization: Bearer t\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// And the exact way it parses the reply, so a response the edge cannot read
	// fails here rather than in production.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read tunnel response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel was not established: status = %s", resp.Status)
	}
	if _, err := strconv.ParseFloat(resp.Header.Get("X-Sandbox-Wake-Seconds"), 64); err != nil {
		t.Fatalf("X-Sandbox-Wake-Seconds not parseable by the edge: %q",
			resp.Header.Get("X-Sandbox-Wake-Seconds"))
	}

	go func() {
		buf := make([]byte, 4)
		_, _ = io.ReadFull(guest, buf)
		_, _ = guest.Write([]byte("pong"))
		_ = guest.Close()
	}()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read tunnel payload: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("tunnel response = %q", got)
	}
}

func TestConnectWebSocketThroughInstrumentedMiddleware(t *testing.T) {
	s := connectTestServer(t)
	if _, err := s.reg.AddURLPort(context.Background(), "sandbox-id", 22); err != nil {
		t.Fatal(err)
	}
	workerBackend, guest := net.Pipe()
	s.pf.dial = func(context.Context, string, int) (net.Conn, error) { return workerBackend, nil }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes/{id}/connect/{port}", s.handleConnectPortWebSocket)
	srv := httptest.NewServer(httpapi.Middleware(mux))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/sandboxes/sandbox-id/connect/22"
	ws, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	defer conn.Close()
	go func() {
		payload := make([]byte, 4)
		_, _ = io.ReadFull(guest, payload)
		_, _ = guest.Write([]byte("pong"))
		_ = guest.Close()
	}()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "pong" {
		t.Fatalf("websocket tunnel response = %q", payload)
	}
}
