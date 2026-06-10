package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ayush6624/web-sandbox/internal/registry"
)

// handleExposePort forwards an extra guest port to a freshly allocated host
// port. Idempotent: exposing an already-mapped guest port returns the
// existing mapping without adding a second DNAT rule.
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
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		httpError(w, 404, err)
		return
	}

	// The primary guest port is always forwarded at create time.
	if body.GuestPort == s.cfg.Provisioner.Network.GuestPort {
		writeJSON(w, 200, registry.PortMapping{GuestPort: body.GuestPort, HostPort: sb.HostPort})
		return
	}

	// An existing mapping already has its DNAT rule — don't add another.
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
		httpError(w, 500, fmt.Errorf("allocate host port: %w", err))
		return
	}
	if err := s.cfg.Provisioner.AddPortForwardTo(hostPort, sb.GuestIP, body.GuestPort); err != nil {
		_ = s.reg.DeletePort(ctx, id, body.GuestPort)
		httpError(w, 500, fmt.Errorf("port forward: %w", err))
		return
	}
	writeJSON(w, 200, registry.PortMapping{GuestPort: body.GuestPort, HostPort: hostPort})
}

// handleListPorts returns every forwarded port of a sandbox, including the
// implicit primary mapping (guest 5173 → the sandbox's host_port).
func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	extra, err := s.reg.Ports(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	out := append(
		[]registry.PortMapping{{GuestPort: s.cfg.Provisioner.Network.GuestPort, HostPort: sb.HostPort}},
		extra...)
	writeJSON(w, 200, out)
}
