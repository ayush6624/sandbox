package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// handleConnectPort upgrades an authenticated worker API connection into an
// opaque TCP tunnel to an explicitly exposed guest port. It intentionally does
// no HTTP parsing after the 200, so WebSockets, SSE, gRPC-over-h1 upgrades, SSH,
// and arbitrary TCP protocols all share one path.
func (s *Server) handleConnectPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPort, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || guestPort < 1 || guestPort > 65535 {
		httpError(w, http.StatusBadRequest, fmt.Errorf("invalid guest port %q", r.PathValue("port")))
		return
	}

	exposed, err := s.portIsExposed(r.Context(), id, guestPort)
	if err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	if !exposed {
		httpError(w, http.StatusNotFound, fmt.Errorf("sandbox %s does not expose guest port %d", id, guestPort))
		return
	}

	// Pin before waking/dialing and keep the pin for the complete connection.
	// The tunnel is therefore identical to a host-port proxy from the reaper's
	// point of view.
	done := s.act.begin(id)
	defer done()
	dialStarted := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), portDialTimeout)
	backend, err := s.pf.dial(ctx, id, guestPort)
	cancel()
	if err != nil {
		status := statusFor(err)
		if status == http.StatusInternalServerError {
			status = http.StatusBadGateway
		}
		capacityOrHTTPError(w, status, fmt.Errorf("dial guest port %d: %w", guestPort, err))
		return
	}
	defer backend.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		httpError(w, http.StatusInternalServerError, errors.New("HTTP server does not support connection hijacking"))
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 200 Connection Established\r\nX-Sandbox-Wake-Seconds: %.6f\r\n\r\n",
		time.Since(dialStarted).Seconds()); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}

	// net/http can buffer bytes beyond the request headers. Preserve them
	// rather than losing a client that optimistically wrote tunnel data.
	front := net.Conn(client)
	if rw.Reader.Buffered() > 0 {
		front = &bufferedConn{Conn: client, r: rw.Reader}
	}
	pipeConns(front, backend)
}

func (s *Server) portIsExposed(ctx context.Context, id string, guestPort int) (bool, error) {
	if _, err := s.reg.Get(ctx, id); err != nil {
		return false, err
	}
	ports, err := s.reg.Ports(ctx, id)
	if err != nil {
		return false, err
	}
	for _, pm := range ports {
		if pm.GuestPort == guestPort {
			return true, nil
		}
	}
	return false, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
