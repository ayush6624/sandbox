package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

type pipeHijacker struct {
	header http.Header
	conn   net.Conn
}

func (w *pipeHijacker) Header() http.Header         { return w.header }
func (w *pipeHijacker) Write(p []byte) (int, error) { return len(p), nil }
func (w *pipeHijacker) WriteHeader(int)             {}
func (w *pipeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func connectTestServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: 5200, PortMax: 5200,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{}, reg)
	t.Cleanup(s.pf.CloseAll)
	if _, err := reg.Create(context.Background(), "sandbox-id", "", "/tmp/rootfs", nil, "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConnectRejectsUnexposedPortBeforeDial(t *testing.T) {
	s := connectTestServer(t)
	called := false
	s.pf.dial = func(context.Context, string, int) (net.Conn, error) {
		called = true
		return nil, nil
	}
	req := httptest.NewRequest(http.MethodConnect, "/sandboxes/sandbox-id/connect/3000", nil)
	req.SetPathValue("id", "sandbox-id")
	req.SetPathValue("port", "3000")
	w := httptest.NewRecorder()
	s.handleConnectPort(w, req)
	if w.Code != http.StatusNotFound || called {
		t.Fatalf("status=%d dialed=%v body=%s", w.Code, called, w.Body)
	}
}

func TestConnectTunnelsOpaqueBytes(t *testing.T) {
	s := connectTestServer(t)
	if _, err := s.reg.AddURLPort(context.Background(), "sandbox-id", 3000); err != nil {
		t.Fatal(err)
	}
	workerBackend, guest := net.Pipe()
	s.pf.dial = func(context.Context, string, int) (net.Conn, error) {
		return workerBackend, nil
	}
	handlerConn, edgeConn := net.Pipe()
	w := &pipeHijacker{header: make(http.Header), conn: handlerConn}
	req := httptest.NewRequest(http.MethodConnect, "/sandboxes/sandbox-id/connect/3000", nil)
	req.SetPathValue("id", "sandbox-id")
	req.SetPathValue("port", "3000")
	done := make(chan struct{})
	go func() {
		s.handleConnectPort(w, req)
		close(done)
	}()

	reader := bufio.NewReader(edgeConn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200 Connection Established") {
		t.Fatalf("status = %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	go func() {
		buf := make([]byte, 4)
		_, _ = io.ReadFull(guest, buf)
		_, _ = guest.Write([]byte("pong"))
		_ = guest.Close()
	}()
	if _, err := edgeConn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "pong" {
		t.Fatalf("tunnel response = %q", got)
	}
	_ = edgeConn.Close()
	<-done
}
