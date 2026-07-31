// Package wsutil lets plain net/http handlers speak just enough WebSocket to
// reject a connection properly. Browsers hide the HTTP status of a failed
// WebSocket handshake — the page sees only an opaque close code 1006 — so an
// auth or routing error on a WS endpoint must be delivered AFTER the upgrade,
// as a close frame carrying a real code and reason. Reject performs the
// minimal 101 handshake and immediately closes with that frame; no WebSocket
// library is needed for it.
package wsutil

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// SubprotocolBearerPrefix carries a bearer credential in the WebSocket
	// subprotocol list. Browsers cannot set request headers on a WebSocket but
	// CAN offer subprotocols, and query-string credentials are deliberately not
	// accepted (they leak into URLs, proxy traces and access logs), so this is
	// the only browser-reachable way to authenticate a WS endpoint.
	//
	// The token is base64url-encoded WITHOUT padding: a subprotocol name must
	// be an RFC 7230 token, and standard base64's '/' and '=' are not tchars —
	// browsers reject such a name at construction time, before any request is
	// sent. Mirrors Kubernetes' base64url.bearer.authorization.k8s.io scheme.
	SubprotocolBearerPrefix = "sandbox.bearer."

	// SubprotocolShell is the credential-free protocol the server selects and
	// echoes in the 101 response. Clients offer it alongside the bearer entry
	// so the handshake completes without reflecting the secret back.
	//
	// Echoing something is mandatory, not cosmetic: a client that offers
	// subprotocols and receives none in the response fails the connection
	// (Chrome reports "Sent non-empty 'Sec-WebSocket-Protocol' header but no
	// response was received"), which would resurface as exactly the opaque
	// 1006 this package exists to prevent.
	SubprotocolShell = "sandbox.shell.v1"
)

const subprotocolHeader = "Sec-WebSocket-Protocol"

// subprotocols returns the offered subprotocol names in order, tolerating both
// repeated header lines and comma-separated values in one line.
func subprotocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values(subprotocolHeader) {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// BearerSubprotocol returns the bearer token offered via the subprotocol list,
// or "" when the request carries none (or an undecodable one).
func BearerSubprotocol(r *http.Request) string {
	for _, p := range subprotocols(r) {
		if !strings.HasPrefix(p, SubprotocolBearerPrefix) {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(p, SubprotocolBearerPrefix))
		if err != nil {
			return ""
		}
		return string(raw)
	}
	return ""
}

// UpgradeAuthorization returns the Authorization header value for r, falling
// back to the subprotocol credential on upgrade requests. Non-upgrade requests
// and requests that already carry a header are returned unchanged, so this can
// front an auth middleware without widening what non-WS routes accept.
func UpgradeAuthorization(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth != "" || !IsUpgrade(r) {
		return auth
	}
	if tok := BearerSubprotocol(r); tok != "" {
		return "Bearer " + tok
	}
	return auth
}

// NegotiatedSubprotocol returns the subprotocol the server must echo, or ""
// when the client offered nothing negotiable.
func NegotiatedSubprotocol(r *http.Request) string {
	if slices.Contains(subprotocols(r), SubprotocolShell) {
		return SubprotocolShell
	}
	return ""
}

// StripBearerSubprotocol removes the credential entry from the offered list so
// a secret never rides past the hop that authenticated it. Other offers (the
// negotiable protocol) are preserved.
func StripBearerSubprotocol(r *http.Request) {
	offered := subprotocols(r)
	kept := make([]string, 0, len(offered))
	for _, p := range offered {
		if !strings.HasPrefix(p, SubprotocolBearerPrefix) {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(offered) {
		return
	}
	if len(kept) == 0 {
		r.Header.Del(subprotocolHeader)
		return
	}
	r.Header.Set(subprotocolHeader, strings.Join(kept, ", "))
}

// EchoSubprotocol completes subprotocol negotiation on a proxied 101 response.
// The in-guest agent does not negotiate subprotocols, so the hop that consumed
// the credential owns finishing the handshake.
func EchoSubprotocol(resp *http.Response, proto string) {
	if proto != "" && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Header.Set(subprotocolHeader, proto)
	}
}

// Close codes sent by Reject. The 4000-4999 range is reserved for private
// use by the WebSocket RFC; these mirror the HTTP status the endpoint would
// have answered with (4000 + status), so clients can map them back.
const (
	CloseUnauthorized = 4401 // missing or invalid bearer token
	CloseNotFound     = 4404 // unknown sandbox id
	CloseInternal     = 4500 // wake or restore failure
	CloseBadGateway   = 4502 // in-guest agent (or owning host) unreachable
)

// CloseCodeFor maps an HTTP status onto the matching 4xxx close code.
func CloseCodeFor(httpStatus int) int {
	return 4000 + httpStatus
}

// IsUpgrade reports whether r is a WebSocket upgrade request.
func IsUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

// magicGUID is the fixed key-derivation constant from RFC 6455 §1.3.
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Reject completes the WebSocket handshake on a request IsUpgrade matched,
// then immediately closes the connection with a close frame carrying code and
// reason. Returns an error when the handshake can't be completed (missing
// Sec-WebSocket-Key, non-hijackable writer) — the caller should fall back to
// a plain HTTP error response then.
func Reject(w http.ResponseWriter, r *http.Request, code int, reason string) error {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return errors.New("missing Sec-WebSocket-Key")
	}
	// http.ResponseController follows Unwrap through instrumentation wrappers
	// (httpapi.Middleware's statusWriter); a direct w.(http.Hijacker) assertion
	// does not, and silently degraded every rejection here to a plain HTTP
	// status — i.e. the opaque 1006 this package exists to avoid.
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return err
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(key + magicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	// A client that offered subprotocols fails the connection unless one is
	// selected, so the rejection has to negotiate before it can be delivered.
	var selected string
	if proto := NegotiatedSubprotocol(r); proto != "" {
		selected = subprotocolHeader + ": " + proto + "\r\n"
	}
	fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n%s\r\n", accept, selected)

	// Close frame: FIN + opcode 8, unmasked (server→client), payload =
	// 2-byte big-endian code + reason, capped at the 125-byte control-frame
	// payload limit.
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	copy(payload[2:], reason)
	buf.Write([]byte{0x88, byte(len(payload))})
	buf.Write(payload)
	if err := buf.Flush(); err != nil {
		return err
	}

	// Give the client a moment to read the frame (and echo its own close)
	// before tearing the TCP connection down.
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = buf.Read(make([]byte, 256))
	return nil
}
