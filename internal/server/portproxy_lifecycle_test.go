package server

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Sync must reconcile a sandbox's listener set inside ONE critical section.
// It used to drop the lock between "close stale" and "open desired", so a
// concurrent CloseSandbox could land in the middle and leave a partially
// reconciled set — or, when the destroy went first, listeners bound for a row
// that no longer exists (leaking those host ports out of the pool for good).
func TestSyncIsAtomicAgainstCloseSandbox(t *testing.T) {
	dial := func(ctx context.Context, id string, port int) (net.Conn, error) {
		return nil, fmt.Errorf("no backend")
	}
	const want = 8
	for iter := 0; iter < 200; iter++ {
		f := testForwarder(t, dial, func(string) func() { return func() {} })
		desired := map[int]int{}
		for i := 0; i < want; i++ {
			desired[freePort(t)] = 8000 + i
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = f.Sync("sb", desired) }()
		go func() { defer wg.Done(); f.CloseSandbox("sb") }()
		wg.Wait()

		f.mu.Lock()
		n := len(f.listeners["sb"])
		f.mu.Unlock()
		// Either the Sync completed and then CloseSandbox tore it all down (0),
		// or CloseSandbox ran first and Sync then established the full set (want).
		// A count in between means the two interleaved mid-reconcile.
		if n != 0 && n != want {
			f.CloseAll()
			t.Fatalf("iteration %d: partially reconciled listener set: %d of %d", iter, n, want)
		}
		f.CloseAll()
	}
}

// A destroy concurrent with DELETE /ports/{port} must never leave a host-port
// listener bound: the registry frees that port for the next sandbox, and a
// leaked listener makes its bind fail with EADDRINUSE. The lifecycle lock is
// what orders the two — handleDeletePort did not take it.
func TestDeletePortConcurrentWithDestroyLeavesNoListener(t *testing.T) {
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
	ctx := context.Background()
	if _, err := s.reg.Create(ctx, "sb", "", "/tmp/rootfs", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create row: %v", err)
	}

	expose := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/sandboxes/sb/ports", strings.NewReader(`{"guest_port":3000}`))
	req.SetPathValue("id", "sb")
	s.handleExposePort(expose, req)
	if expose.Code != 200 {
		t.Fatalf("expose: %d %s", expose.Code, expose.Body)
	}
	s.pf.mu.Lock()
	bound := len(s.pf.listeners["sb"])
	s.pf.mu.Unlock()
	if bound != 1 {
		t.Fatalf("expected 1 bound listener after expose, got %d", bound)
	}

	// Race the port deletion against a full destroy of the sandbox.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		del := httptest.NewRecorder()
		r := httptest.NewRequest("DELETE", "/sandboxes/sb/ports/3000", nil)
		r.SetPathValue("id", "sb")
		r.SetPathValue("port", "3000")
		s.handleDeletePort(del, r)
	}()
	go func() {
		defer wg.Done()
		// destroyLocked's port-teardown sequence, under the same lifecycle lock
		// it holds in production (open-coded so the test needs no Provisioner).
		mu := s.wakeLock("sb")
		mu.Lock()
		defer mu.Unlock()
		s.pf.CloseSandbox("sb")
		_ = s.reg.Destroy(context.Background(), "sb")
	}()
	wg.Wait()

	s.pf.mu.Lock()
	left := len(s.pf.listeners["sb"])
	s.pf.mu.Unlock()
	if left != 0 {
		t.Fatalf("LEAKED %d listener(s) for a destroyed sandbox — host port %d is now unusable", left, hostPort)
	}
	// Prove the port is genuinely reusable: binding it must succeed.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(hostPort)))
	if err != nil {
		t.Fatalf("host port %d still held after destroy: %v", hostPort, err)
	}
	ln.Close()
}
