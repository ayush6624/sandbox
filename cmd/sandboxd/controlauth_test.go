package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestHostOnlyAcceptsOnlyDefaultGateway(t *testing.T) {
	old := defaultGateway
	defaultGateway = func() (netip.Addr, error) {
		return netip.MustParseAddr("172.16.0.1"), nil
	}
	t.Cleanup(func() { defaultGateway = old })

	called := false
	handler := hostOnly(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/identity", nil)
	req.RemoteAddr = "172.16.0.1:4242"
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusNoContent || !called {
		t.Fatalf("gateway request = status %d, called %v", w.Code, called)
	}

	called = false
	req = httptest.NewRequest(http.MethodPost, "/identity", nil)
	req.RemoteAddr = "127.0.0.1:4242"
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("guest-local request = status %d, called %v", w.Code, called)
	}
}
