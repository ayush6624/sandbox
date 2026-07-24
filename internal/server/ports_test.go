package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

func TestPortsAreExplicitOnly(t *testing.T) {
	hostPort := freePort(t)
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 1,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10",
		PortMin: hostPort, PortMax: hostPort,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{}, reg)
	t.Cleanup(s.pf.CloseAll)
	if _, err := s.reg.Create(context.Background(), "sb", "", "/tmp/rootfs", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create row: %v", err)
	}

	list := httptest.NewRecorder()
	listReq := httptest.NewRequest("GET", "/sandboxes/sb/ports", nil)
	listReq.SetPathValue("id", "sb")
	s.handleListPorts(list, listReq)
	if list.Code != 200 || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("new sandbox ports = %d %s, want 200 []", list.Code, list.Body)
	}

	expose := httptest.NewRecorder()
	exposeReq := httptest.NewRequest("POST", "/sandboxes/sb/ports", strings.NewReader(`{"guest_port":3000}`))
	exposeReq.SetPathValue("id", "sb")
	s.handleExposePort(expose, exposeReq)
	if expose.Code != 200 {
		t.Fatalf("expose 3000: %d %s", expose.Code, expose.Body)
	}
	var got registry.PortMapping
	if err := json.Unmarshal(expose.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode mapping: %v", err)
	}
	if got.GuestPort != 3000 || got.HostPort == 0 {
		t.Fatalf("mapping = %+v, want explicit guest 3000 with allocated host port", got)
	}
}
