package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/registry"
)

// A worker configured with default_url_only returns a URL-only mapping for an
// expose that does not ask for a host port. SSH has nothing to dial in that
// case, so ensureSSHPort must demand a host port explicitly instead of
// returning HostPort 0 and letting ssh connect to port 0.
func TestEnsureSSHPortDemandsHostPortUnderDefaultURLOnly(t *testing.T) {
	var exposeBodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ports"):
			_ = json.NewEncoder(w).Encode([]registry.PortMapping{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ports"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			exposeBodies = append(exposeBodies, body)
			// Mimic default_url_only: honour an explicit host_port:true, and
			// otherwise hand back a URL-only mapping.
			if hp, ok := body["host_port"].(bool); ok && hp {
				_ = json.NewEncoder(w).Encode(registry.PortMapping{
					GuestPort: 22, HostPort: 5231, Mode: "host_port",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(registry.PortMapping{
				GuestPort: 22, Mode: "url", URL: "https://22-abc.example.com",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := client.NewHTTP(strings.TrimPrefix(srv.URL, "http://"), "")
	port, err := ensureSSHPort(context.Background(), c, "abc")
	if err != nil {
		t.Fatalf("ensureSSHPort: %v", err)
	}
	if port != 5231 {
		t.Fatalf("ensureSSHPort = %d, want the allocated host port 5231", port)
	}
	if len(exposeBodies) != 1 {
		t.Fatalf("expected exactly one expose call, got %d", len(exposeBodies))
	}
	if hp, ok := exposeBodies[0]["host_port"].(bool); !ok || !hp {
		t.Fatalf("expose body did not request a host port: %v", exposeBodies[0])
	}
}

// An existing URL-only mapping for :22 must be upgraded, not reported as the
// host port 0 it carries.
func TestEnsureSSHPortUpgradesExistingURLOnlyMapping(t *testing.T) {
	upgraded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ports"):
			_ = json.NewEncoder(w).Encode([]registry.PortMapping{
				{GuestPort: 22, Mode: "url", URL: "https://22-abc.example.com"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ports"):
			upgraded = true
			_ = json.NewEncoder(w).Encode(registry.PortMapping{
				GuestPort: 22, HostPort: 5232, Mode: "both",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := client.NewHTTP(strings.TrimPrefix(srv.URL, "http://"), "")
	port, err := ensureSSHPort(context.Background(), c, "abc")
	if err != nil {
		t.Fatalf("ensureSSHPort: %v", err)
	}
	if !upgraded {
		t.Fatal("a URL-only :22 mapping was accepted without upgrading it to a host port")
	}
	if port != 5232 {
		t.Fatalf("ensureSSHPort = %d, want 5232", port)
	}
}

// An existing host-port mapping is reused without a second expose call.
func TestEnsureSSHPortReusesExistingHostPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("expose called even though :22 already had a host port")
		}
		_ = json.NewEncoder(w).Encode([]registry.PortMapping{
			{GuestPort: 22, HostPort: 5233, Mode: "host_port"},
		})
	}))
	defer srv.Close()

	c := client.NewHTTP(strings.TrimPrefix(srv.URL, "http://"), "")
	port, err := ensureSSHPort(context.Background(), c, "abc")
	if err != nil {
		t.Fatalf("ensureSSHPort: %v", err)
	}
	if port != 5233 {
		t.Fatalf("ensureSSHPort = %d, want 5233", port)
	}
}

// If a host somehow returns no host port even when asked, fail loudly rather
// than handing ssh port 0.
func TestEnsureSSHPortErrorsRatherThanReturningZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]registry.PortMapping{})
			return
		}
		_ = json.NewEncoder(w).Encode(registry.PortMapping{GuestPort: 22, Mode: "url"})
	}))
	defer srv.Close()

	c := client.NewHTTP(strings.TrimPrefix(srv.URL, "http://"), "")
	port, err := ensureSSHPort(context.Background(), c, "abc")
	if err == nil {
		t.Fatalf("expected an error, got port %d", port)
	}
	if port != 0 {
		t.Fatalf("port = %d on error path", port)
	}
}
