package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ayush6624/sandbox/internal/registry"
)

// handleExposePort forwards a guest port to a freshly allocated host
// port. Idempotent: exposing an already-mapped guest port returns the
// existing mapping without opening a second listener.
func (s *Server) handleExposePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		GuestPort int   `json:"guest_port"`
		HostPort  *bool `json:"host_port"`
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
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	// The sandbox may have been destroyed or released while this request was
	// waiting for the lifecycle lock.
	if _, err := s.reg.Get(ctx, id); err != nil {
		httpError(w, statusFor(err), err)
		return
	}

	// An existing mapping already has its listener — don't open another.
	existing, err := s.reg.Ports(ctx, id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	wantHost := !s.cfg.DefaultURLOnly
	if body.HostPort != nil {
		wantHost = *body.HostPort
	}
	if !wantHost && strings.Trim(strings.TrimSpace(s.cfg.IngressDomain), ".") == "" {
		httpError(w, http.StatusBadRequest, fmt.Errorf("URL-only exposure requires ingress_domain"))
		return
	}
	existingURLOnly := false
	for _, pm := range existing {
		if pm.GuestPort == body.GuestPort {
			if wantHost && pm.HostPort == 0 {
				existingURLOnly = true
				break // upgrade the URL-only mapping below
			}
			s.decoratePorts(id, existing)
			for _, decorated := range existing {
				if decorated.GuestPort == body.GuestPort {
					pm = decorated
				}
			}
			writeJSON(w, 200, pm)
			return
		}
	}

	var pm registry.PortMapping
	if wantHost {
		hostPort, err := s.reg.AddPort(ctx, id, body.GuestPort)
		pm = registry.PortMapping{GuestPort: body.GuestPort, HostPort: hostPort, Mode: "host_port"}
		if err == nil {
			err = s.pf.Open(id, hostPort, body.GuestPort)
			if err != nil {
				_ = s.reg.DeletePort(ctx, id, body.GuestPort)
				if existingURLOnly {
					_, _ = s.reg.AddURLPort(ctx, id, body.GuestPort)
				}
				err = fmt.Errorf("port forward: %w", err)
			}
		}
	} else {
		pm, err = s.reg.AddURLPort(ctx, id, body.GuestPort)
	}
	if err != nil {
		capacityOrHTTPError(w, 500, fmt.Errorf("expose port: %w", err))
		return
	}
	decorated := []registry.PortMapping{pm}
	s.decoratePorts(id, decorated)
	pm = decorated[0]
	writeJSON(w, 200, pm)
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
	s.decoratePorts(id, ports)
	writeJSON(w, 200, ports)
}

func (s *Server) handleDeletePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPort, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || guestPort < 1 || guestPort > 65535 {
		httpError(w, http.StatusBadRequest, fmt.Errorf("invalid guest port %q", r.PathValue("port")))
		return
	}
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	done := s.act.begin(id)
	defer done()
	if err := s.reg.DeletePort(r.Context(), id, guestPort); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if sb, err := s.reg.Get(r.Context(), id); err == nil {
		if err := s.syncSandboxPorts(r.Context(), sb); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetPublicPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPort, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || guestPort < 1 || guestPort > 65535 {
		httpError(w, http.StatusBadRequest, fmt.Errorf("invalid guest port %q", r.PathValue("port")))
		return
	}
	var body struct {
		PublicPort int `json:"public_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.PublicPort < 1 || body.PublicPort > 65535 {
		httpError(w, http.StatusBadRequest, fmt.Errorf("invalid public_port %d", body.PublicPort))
		return
	}
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	done := s.act.begin(id)
	defer done()
	ports, err := s.reg.Ports(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	exists := false
	for _, pm := range ports {
		if pm.GuestPort == guestPort {
			exists = true
			break
		}
	}
	created := false
	if !exists {
		if _, err := s.reg.AddURLPort(r.Context(), id, guestPort); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		created = true
	}
	if err := s.reg.SetPublicPort(r.Context(), id, guestPort, body.PublicPort); err != nil {
		if created {
			_ = s.reg.DeletePort(r.Context(), id, guestPort)
		}
		httpError(w, http.StatusConflict, err)
		return
	}
	ports, err = s.reg.Ports(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	for _, pm := range ports {
		if pm.GuestPort == guestPort {
			s.decoratePorts(id, ports)
			for _, decorated := range ports {
				if decorated.GuestPort == guestPort {
					pm = decorated
				}
			}
			writeJSON(w, http.StatusOK, pm)
			return
		}
	}
	httpError(w, http.StatusNotFound, fmt.Errorf("port mapping disappeared"))
}

func (s *Server) decoratePorts(id string, ports []registry.PortMapping) {
	domain := strings.Trim(strings.TrimSpace(s.cfg.IngressDomain), ".")
	if domain == "" {
		return
	}
	for i := range ports {
		ports[i].URL = fmt.Sprintf("https://%d-%s.%s", ports[i].GuestPort, id, domain)
		if ports[i].HostPort != 0 {
			ports[i].Mode = "both"
		} else if ports[i].PublicPort != 0 {
			ports[i].Mode = "raw"
		}
	}
}
