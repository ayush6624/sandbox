package wsutil

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		upgrade    string
		connection string
		want       bool
	}{
		{"websocket upgrade", "websocket", "Upgrade", true},
		{"case-insensitive", "WebSocket", "keep-alive, Upgrade", true},
		{"plain request", "", "", false},
		{"upgrade to something else", "h2c", "Upgrade", false},
		{"upgrade header without connection", "websocket", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/shell", nil)
			if tc.upgrade != "" {
				r.Header.Set("Upgrade", tc.upgrade)
			}
			if tc.connection != "" {
				r.Header.Set("Connection", tc.connection)
			}
			if got := IsUpgrade(r); got != tc.want {
				t.Fatalf("IsUpgrade = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReject drives a real TCP connection through a handshake + Reject and
// asserts the client sees a valid 101 followed by a close frame carrying the
// code and reason — the exact bytes a browser needs to surface the error.
func TestReject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Reject(w, r, CloseUnauthorized, "missing or invalid bearer token"); err != nil {
			t.Errorf("Reject: %v", err)
		}
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	const key = "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 §1.3 example key
	fmt.Fprintf(conn, "GET /shell HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", key)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	sum := sha1.Sum([]byte(key + magicGUID))
	if want := base64.StdEncoding.EncodeToString(sum[:]); resp.Header.Get("Sec-WebSocket-Accept") != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", resp.Header.Get("Sec-WebSocket-Accept"), want)
	}

	header := make([]byte, 2)
	if _, err := io.ReadFull(br, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if header[0] != 0x88 {
		t.Fatalf("frame byte0 = %#x, want 0x88 (FIN + close)", header[0])
	}
	payload := make([]byte, header[1]&0x7f)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	if code := binary.BigEndian.Uint16(payload); code != CloseUnauthorized {
		t.Fatalf("close code = %d, want %d", code, CloseUnauthorized)
	}
	if reason := string(payload[2:]); reason != "missing or invalid bearer token" {
		t.Fatalf("close reason = %q", reason)
	}
}

func TestRejectWithoutKeyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Reject(w, r, CloseNotFound, "nope"); err == nil {
			t.Error("Reject without Sec-WebSocket-Key should fail")
			return
		}
		// Caller falls back to a plain HTTP error.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fallback status = %d, want 404", resp.StatusCode)
	}
}

func TestCloseCodeFor(t *testing.T) {
	if got := CloseCodeFor(http.StatusBadGateway); got != CloseBadGateway {
		t.Fatalf("CloseCodeFor(502) = %d, want %d", got, CloseBadGateway)
	}
}
