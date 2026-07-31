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
	"strings"
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

// wrappedWriter mirrors httpapi.Middleware's statusWriter: it embeds the
// ResponseWriter *interface* (so no Hijack method is promoted) and exposes
// Unwrap for http.ResponseController. A direct w.(http.Hijacker) assertion
// fails on this, which silently turned every WS rejection behind the
// instrumentation middleware into an opaque 1006.
type wrappedWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrappedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *wrappedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestRejectThroughWrappedWriter is the regression guard: Reject must still
// deliver a close frame when the writer is wrapped by request instrumentation.
func TestRejectThroughWrappedWriter(t *testing.T) {
	if _, ok := (&wrappedWriter{}).ResponseWriter.(http.Hijacker); ok {
		t.Fatal("test fixture is hijackable by assertion; it must not be")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Reject(&wrappedWriter{ResponseWriter: w}, r, CloseNotFound, "sandbox gone"); err != nil {
			t.Errorf("Reject through wrapper: %v", err)
		}
	}))
	defer srv.Close()

	code, reason, proto := rejectRoundTrip(t, srv.Listener.Addr().String(), nil)
	if code != CloseNotFound {
		t.Fatalf("close code = %d, want %d", code, CloseNotFound)
	}
	if reason != "sandbox gone" {
		t.Fatalf("close reason = %q", reason)
	}
	if proto != "" {
		t.Fatalf("echoed subprotocol = %q, want none (client offered none)", proto)
	}
}

// TestRejectEchoesNegotiatedSubprotocol pins the handshake half of the fix: a
// client that offered subprotocols must be answered with one, or it fails the
// connection before it can ever read the close frame.
func TestRejectEchoesNegotiatedSubprotocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := Reject(w, r, CloseUnauthorized, "bad token"); err != nil {
			t.Errorf("Reject: %v", err)
		}
	}))
	defer srv.Close()

	offer := []string{SubprotocolBearerPrefix + "abc", SubprotocolShell}
	code, _, proto := rejectRoundTrip(t, srv.Listener.Addr().String(), offer)
	if code != CloseUnauthorized {
		t.Fatalf("close code = %d, want %d", code, CloseUnauthorized)
	}
	if proto != SubprotocolShell {
		t.Fatalf("echoed subprotocol = %q, want %q", proto, SubprotocolShell)
	}
}

// rejectRoundTrip performs an upgrade against addr and returns the close code,
// reason, and echoed subprotocol.
func rejectRoundTrip(t *testing.T, addr string, offer []string) (uint16, string, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	req := fmt.Sprintf("GET /shell HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	if len(offer) > 0 {
		req += subprotocolHeader + ": " + strings.Join(offer, ", ") + "\r\n"
	}
	fmt.Fprint(conn, req+"\r\n")

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
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
	return binary.BigEndian.Uint16(payload), string(payload[2:]), resp.Header.Get(subprotocolHeader)
}

func TestSubprotocolCredentials(t *testing.T) {
	// base64url, unpadded — a token with bytes that standard base64 would
	// render as '/' or '=' must survive, since those are not RFC 7230 tchars
	// and a browser rejects such a subprotocol name outright.
	const token = "s3cr3t-tok/en+value=="
	encoded := base64.RawURLEncoding.EncodeToString([]byte(token))
	if strings.ContainsAny(encoded, "/+=") {
		t.Fatalf("encoded token %q still contains non-token characters", encoded)
	}

	newUpgrade := func(offers ...string) *http.Request {
		r := httptest.NewRequest("GET", "/sandboxes/x/shell", nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
		for _, o := range offers {
			r.Header.Add(subprotocolHeader, o)
		}
		return r
	}

	t.Run("extracts and authorizes", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix+encoded, SubprotocolShell)
		if got := BearerSubprotocol(r); got != token {
			t.Fatalf("BearerSubprotocol = %q, want %q", got, token)
		}
		if got := UpgradeAuthorization(r); got != "Bearer "+token {
			t.Fatalf("UpgradeAuthorization = %q", got)
		}
		if got := NegotiatedSubprotocol(r); got != SubprotocolShell {
			t.Fatalf("NegotiatedSubprotocol = %q", got)
		}
	})

	t.Run("comma-separated single header", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix + encoded + ", " + SubprotocolShell)
		if got := BearerSubprotocol(r); got != token {
			t.Fatalf("BearerSubprotocol = %q, want %q", got, token)
		}
		if got := NegotiatedSubprotocol(r); got != SubprotocolShell {
			t.Fatalf("NegotiatedSubprotocol = %q", got)
		}
	})

	t.Run("an existing header wins", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix + encoded)
		r.Header.Set("Authorization", "Bearer header-token")
		if got := UpgradeAuthorization(r); got != "Bearer header-token" {
			t.Fatalf("UpgradeAuthorization = %q, want the header value", got)
		}
	})

	t.Run("non-upgrade requests are never credentialed by subprotocol", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/sandboxes", nil)
		r.Header.Add(subprotocolHeader, SubprotocolBearerPrefix+encoded)
		if got := UpgradeAuthorization(r); got != "" {
			t.Fatalf("UpgradeAuthorization on a plain request = %q, want empty", got)
		}
	})

	t.Run("undecodable credential is not authorization", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix + "not!base64url")
		if got := BearerSubprotocol(r); got != "" {
			t.Fatalf("BearerSubprotocol = %q, want empty", got)
		}
		if got := UpgradeAuthorization(r); got != "" {
			t.Fatalf("UpgradeAuthorization = %q, want empty", got)
		}
	})

	t.Run("strip removes only the credential", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix+encoded, SubprotocolShell)
		StripBearerSubprotocol(r)
		if got := r.Header.Get(subprotocolHeader); got != SubprotocolShell {
			t.Fatalf("after strip = %q, want %q", got, SubprotocolShell)
		}
		if got := BearerSubprotocol(r); got != "" {
			t.Fatal("credential survived the strip")
		}
	})

	t.Run("strip drops the header when only the credential was offered", func(t *testing.T) {
		r := newUpgrade(SubprotocolBearerPrefix + encoded)
		StripBearerSubprotocol(r)
		if got := r.Header.Values(subprotocolHeader); len(got) != 0 {
			t.Fatalf("header survived: %q", got)
		}
	})
}

func TestEchoSubprotocol(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		proto  string
		want   string
	}{
		{"101 with a protocol", http.StatusSwitchingProtocols, SubprotocolShell, SubprotocolShell},
		{"101 with none negotiated", http.StatusSwitchingProtocols, "", ""},
		{"non-upgrade response untouched", http.StatusOK, SubprotocolShell, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: make(http.Header)}
			EchoSubprotocol(resp, tc.proto)
			if got := resp.Header.Get(subprotocolHeader); got != tc.want {
				t.Fatalf("echoed = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloseCodeFor(t *testing.T) {
	if got := CloseCodeFor(http.StatusBadGateway); got != CloseBadGateway {
		t.Fatalf("CloseCodeFor(502) = %d, want %d", got, CloseBadGateway)
	}
}
