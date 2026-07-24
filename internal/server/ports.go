package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ayush6624/sandbox/internal/registry"
)

// handleExposePort forwards a guest port to a freshly allocated host
// port. Idempotent: exposing an already-mapped guest port returns the
// existing mapping without opening a second listener.
func (s *Server) handleExposePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		GuestPort int `json:"guest_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.GuestPort < 1 || body.GuestPort > 65535 {
		httpError(w, 400, fmt.Errorf("invalid guest_port %d", body.GuestPort))
		return
	}

	ctx := r.Context()
	// The proxy listener resolves the guest IP per connection, so exposing a
	// port needs no live guest — a hibernated sandbox stays frozen and the
	// new port simply becomes another wake-on-connect entry point.
	_, err := s.reg.Get(ctx, id)
	if err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	done := s.act.begin(id)
	defer done()

	// An existing mapping already has its listener — don't open another.
	existing, err := s.reg.Ports(ctx, id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	for _, pm := range existing {
		if pm.GuestPort == body.GuestPort {
			writeJSON(w, 200, pm)
			return
		}
	}

	hostPort, err := s.reg.AddPort(ctx, id, body.GuestPort)
	if err != nil {
		capacityOrHTTPError(w, 500, fmt.Errorf("allocate host port: %w", err))
		return
	}
	if err := s.pf.Open(id, hostPort, body.GuestPort); err != nil {
		_ = s.reg.DeletePort(ctx, id, body.GuestPort)
		httpError(w, 500, fmt.Errorf("port forward: %w", err))
		return
	}
	writeJSON(w, 200, registry.PortMapping{GuestPort: body.GuestPort, HostPort: hostPort})
}

// handleListPorts returns every explicitly forwarded port of a sandbox.
func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	ports, err := s.reg.Ports(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if ports == nil {
		ports = []registry.PortMapping{}
	}
	writeJSON(w, 200, ports)
}
