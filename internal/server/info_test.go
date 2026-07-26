package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// testServerWithTemplate mirrors testServer but sets template resources, so
// effective-resource reporting has defaults to fill in.
func testServerWithTemplate(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	return New(Config{
		VMTemplate:     vm.RunOptions{Vcpus: 2, MemMIB: 1024},
		HotCreate:      true,
		HibernateAfter: 90 * time.Second,
	}, reg)
}

func TestEffectiveResourcesFillsTemplateDefaults(t *testing.T) {
	s := testServerWithTemplate(t)

	got := s.effectiveResources(registry.Sandbox{})
	if got.Vcpus != 2 || got.MemMIB != 1024 {
		t.Fatalf("unset resources should report template defaults, got vcpus=%d mem=%d", got.Vcpus, got.MemMIB)
	}

	got = s.effectiveResources(registry.Sandbox{Vcpus: 4, MemMIB: 2048})
	if got.Vcpus != 4 || got.MemMIB != 2048 {
		t.Fatalf("overrides must pass through untouched, got vcpus=%d mem=%d", got.Vcpus, got.MemMIB)
	}
}

// TestListAndGetReportEffectiveResources exercises the handlers end to end:
// a row stored with 0 (= template default) must serialize with the template's
// values, and an overridden row with its own.
func TestListAndGetReportEffectiveResources(t *testing.T) {
	s := testServerWithTemplate(t)
	ctx := context.Background()

	def, err := s.reg.Create(ctx, "sb-default", "", "/tmp/r1", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create default row: %v", err)
	}
	ovr, err := s.reg.Create(ctx, "sb-override", "", "/tmp/r2", nil, "", 0, 4, 2048)
	if err != nil {
		t.Fatalf("create override row: %v", err)
	}

	w := httptest.NewRecorder()
	s.handleList(w, httptest.NewRequest("GET", "/sandboxes", nil))
	if w.Code != 200 {
		t.Fatalf("list: %d (%s)", w.Code, w.Body)
	}
	var listed []registry.Sandbox
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if strings.Contains(w.Body.String(), `"host_port"`) {
		t.Fatalf("sandbox response must not contain an implicit host port: %s", w.Body)
	}
	want := map[string][2]int64{def.ID: {2, 1024}, ovr.ID: {4, 2048}}
	for _, sb := range listed {
		if sb.Vcpus != want[sb.ID][0] || sb.MemMIB != want[sb.ID][1] {
			t.Fatalf("list row %s: vcpus=%d mem=%d, want %v", sb.ID, sb.Vcpus, sb.MemMIB, want[sb.ID])
		}
	}

	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sandboxes/"+def.ID, nil)
	r.SetPathValue("id", def.ID)
	s.handleGet(w, r)
	var got registry.Sandbox
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Vcpus != 2 || got.MemMIB != 1024 {
		t.Fatalf("get: vcpus=%d mem=%d, want template 2/1024", got.Vcpus, got.MemMIB)
	}
	// The registry row itself must keep the 0 sentinel — only responses fill.
	stored, err := s.reg.Get(ctx, def.ID)
	if err != nil {
		t.Fatalf("reg get: %v", err)
	}
	if stored.Vcpus != 0 || stored.MemMIB != 0 {
		t.Fatalf("stored row mutated: vcpus=%d mem=%d, want 0/0", stored.Vcpus, stored.MemMIB)
	}
}

func TestHandleInfo(t *testing.T) {
	s := testServerWithTemplate(t)
	w := httptest.NewRecorder()
	s.handleInfo(w, httptest.NewRequest("GET", "/info", nil))
	if w.Code != 200 {
		t.Fatalf("info: %d (%s)", w.Code, w.Body)
	}
	var info Info
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if strings.Contains(w.Body.String(), `"guest_port"`) {
		t.Fatalf("host info must not advertise an implicit guest port: %s", w.Body)
	}
	if info.DefaultVcpus != 2 || info.DefaultMemMIB != 1024 {
		t.Fatalf("defaults: vcpus=%d mem=%d, want 2/1024", info.DefaultVcpus, info.DefaultMemMIB)
	}
	if info.MaxVcpus < 1 || info.MaxMemMIB < minMemMIB {
		t.Fatalf("limits should be positive: max_vcpus=%d max_mem=%d", info.MaxVcpus, info.MaxMemMIB)
	}
	if !info.HotCreate || info.HibernateAfterSec != 90 {
		t.Fatalf("flags: hot_create=%v hibernate_after_sec=%d", info.HotCreate, info.HibernateAfterSec)
	}
}

// TestBearerAuthWebSocket verifies that credentials never ride in query
// strings and that worker-only routes reject client credentials.
func TestBearerAuthWebSocket(t *testing.T) {
	const clientToken = "client-sekrit"
	const workerToken = "worker-sekrit"
	clientCreds, err := management.NewCredentials([]string{clientToken}, "")
	if err != nil {
		t.Fatal(err)
	}
	workerCreds, err := management.NewCredentials([]string{workerToken}, "")
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	handler := bearerAuth(clientCreds, workerCreds, next)

	t.Run("query token rejected on upgrade", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/sandboxes/x/shell?access_token="+clientToken, nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", w.Code)
		}
	})

	t.Run("authorization accepted on upgrade", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/sandboxes/x/shell", nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Authorization", "Bearer "+clientToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", w.Code)
		}
	})

	t.Run("bad token on upgrade closes with 4401", func(t *testing.T) {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "GET /sandboxes/x/shell?access_token=wrong HTTP/1.1\r\nHost: x\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != 101 {
			t.Fatalf("status = %d, want 101 (close frame carries the error)", resp.StatusCode)
		}
		header := make([]byte, 2)
		if _, err := io.ReadFull(br, header); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		payload := make([]byte, header[1]&0x7f)
		if _, err := io.ReadFull(br, payload); err != nil {
			t.Fatalf("read payload: %v", err)
		}
		if code := binary.BigEndian.Uint16(payload); code != wsutil.CloseUnauthorized {
			t.Fatalf("close code = %d, want %d", code, wsutil.CloseUnauthorized)
		}
		if !strings.Contains(string(payload[2:]), "bearer token") {
			t.Fatalf("close reason = %q, should mention the token", payload[2:])
		}
	})

	t.Run("client token cannot call internal route", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/internal/v1/sandboxes/x:adopt", nil)
		r.Header.Set("Authorization", "Bearer "+clientToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("client internal status = %d", w.Code)
		}
		r = httptest.NewRequest("POST", "/internal/v1/sandboxes/x:adopt", nil)
		r.Header.Set("Authorization", "Bearer "+workerToken)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("worker internal status = %d", w.Code)
		}
	})

	t.Run("worker token cannot call client route", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
		r.Header.Set("Authorization", "Bearer "+workerToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("worker client-route status = %d", w.Code)
		}
	})
}

func TestBearerAuthRejectsOverlappingTokenOnInternalRoute(t *testing.T) {
	clientCreds, err := management.NewCredentials([]string{"client-next", "shared-old"}, "")
	if err != nil {
		t.Fatal(err)
	}
	workerCreds, err := management.NewCredentials([]string{"worker-next", "shared-old"}, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := bearerAuth(clientCreds, workerCreds, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/sandboxes/x:adopt", nil)
	req.Header.Set("Authorization", "Bearer shared-old")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("overlapping token reached internal route: status=%d", w.Code)
	}
}

func TestSecureUnixSocketMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}

	err = secureUnixSocket(path)
	if os.Geteuid() != 0 {
		if err == nil || !strings.Contains(err.Error(), "requires root ownership") {
			t.Fatalf("non-root secureUnixSocket error = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %04o, want 0600", got)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("socket stat type = %T", info.Sys())
	}
	if stat.Uid != 0 || stat.Gid != 0 {
		t.Fatalf("socket owner = %d:%d, want 0:0", stat.Uid, stat.Gid)
	}
}
