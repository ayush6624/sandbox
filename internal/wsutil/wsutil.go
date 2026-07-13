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
	"strings"
	"time"
)

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
	hj, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("response writer is not hijackable")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return err
	}
	defer conn.Close()

	sum := sha1.Sum([]byte(key + magicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", accept)

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
