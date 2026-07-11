package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend is a line-echo TCP server standing in for a guest service: it
// answers each received line with "echo:<line>".
type fakeBackend struct {
	ln net.Listener
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &fakeBackend{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					fmt.Fprintf(conn, "echo:%s\n", sc.Text())
				}
			}()
		}
	}()
	return b
}

func (b *fakeBackend) addr() string { return b.ln.Addr().String() }

// trackCounter is a fake activity tracker counting open/closed connections.
type trackCounter struct {
	begun, done atomic.Int64
}

func (tc *trackCounter) track(string) func() {
	tc.begun.Add(1)
	return func() { tc.done.Add(1) }
}

// freePort grabs an ephemeral port for the forwarder to bind. Racy in theory,
// fine in practice for tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func testForwarder(t *testing.T, dial dialGuestFunc, track func(string) func()) *portForwarder {
	t.Helper()
	f := newPortForwarder(dial, track)
	f.bind = "127.0.0.1"
	t.Cleanup(f.CloseAll)
	return f
}

// roundTrip connects to 127.0.0.1:hostPort, sends one line, and returns the
// reply line.
func roundTrip(t *testing.T, hostPort int, msg string) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSuffix(line, "\n"), err
}

func TestPortForwarderProxiesToBackend(t *testing.T) {
	backend := newFakeBackend(t)
	var dialed atomic.Int64
	var gotID atomic.Value
	var gotGuestPort atomic.Int64
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		dialed.Add(1)
		gotID.Store(id)
		gotGuestPort.Store(int64(guestPort))
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backend.addr())
	}
	tc := &trackCounter{}
	f := testForwarder(t, dial, tc.track)

	hostPort := freePort(t)
	if err := f.Open("sb1", hostPort, 3000); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Idempotent re-open of the same mapping (the wake path does this).
	if err := f.Open("sb1", hostPort, 3000); err != nil {
		t.Fatalf("re-open same mapping: %v", err)
	}

	reply, err := roundTrip(t, hostPort, "hello")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if reply != "echo:hello" {
		t.Fatalf("reply = %q, want echo:hello", reply)
	}
	if dialed.Load() != 1 {
		t.Fatalf("dial hook called %d times, want 1", dialed.Load())
	}
	if gotID.Load() != "sb1" || gotGuestPort.Load() != 3000 {
		t.Fatalf("dial hook got (%v, %d), want (sb1, 3000)", gotID.Load(), gotGuestPort.Load())
	}

	// The connection is closed → activity must be released.
	waitFor(t, "activity released", func() bool {
		return tc.begun.Load() == 1 && tc.done.Load() == 1
	})
}

func TestPortForwarderWakesAndRedialsCurrentBackend(t *testing.T) {
	// Two backends stand in for the guest IP changing across a clone-path
	// wake: the dial hook must be consulted per connection and reach whatever
	// the CURRENT backend is — never a cached address.
	backendA := newFakeBackend(t)
	backendB := newFakeBackend(t)

	var mu sync.Mutex
	state := "hibernated"
	current := backendA.addr()
	wakes := 0
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		mu.Lock()
		if state == "hibernated" { // the ensureRunning stand-in: wake first
			state = "running"
			wakes++
			current = backendB.addr() // wake came back with a fresh identity
		}
		addr := current
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	tc := &trackCounter{}
	f := testForwarder(t, dial, tc.track)

	hostPort := freePort(t)
	if err := f.Open("sb1", hostPort, 3000); err != nil {
		t.Fatalf("open: %v", err)
	}

	// First connection hits a hibernated sandbox: it must wake it and land on
	// the post-wake backend.
	if reply, err := roundTrip(t, hostPort, "wake-me"); err != nil || reply != "echo:wake-me" {
		t.Fatalf("round trip = (%q, %v), want echo through the post-wake backend", reply, err)
	}
	mu.Lock()
	if state != "running" || wakes != 1 {
		mu.Unlock()
		t.Fatalf("connection must wake the sandbox exactly once (state=%s wakes=%d)", state, wakes)
	}
	mu.Unlock()

	// Second connection: already running, no second wake.
	if reply, err := roundTrip(t, hostPort, "again"); err != nil || reply != "echo:again" {
		t.Fatalf("second round trip = (%q, %v)", reply, err)
	}
	mu.Lock()
	if wakes != 1 {
		mu.Unlock()
		t.Fatalf("running sandbox must not be woken again (wakes=%d)", wakes)
	}
	mu.Unlock()
}

func TestPortForwarderDialFailureClosesClient(t *testing.T) {
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		return nil, errors.New("wake failed")
	}
	tc := &trackCounter{}
	f := testForwarder(t, dial, tc.track)

	hostPort := freePort(t)
	if err := f.Open("sb1", hostPort, 3000); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := roundTrip(t, hostPort, "doomed"); err == nil {
		t.Fatal("client must see the connection close when the wake/dial fails")
	}
	// Activity must balance even on the failure path.
	waitFor(t, "activity released after dial failure", func() bool {
		return tc.begun.Load() == 1 && tc.done.Load() == 1
	})
}

func TestPortForwarderCloseSandboxStopsListening(t *testing.T) {
	backend := newFakeBackend(t)
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backend.addr())
	}
	tc := &trackCounter{}
	f := testForwarder(t, dial, tc.track)

	hostPort := freePort(t)
	if err := f.Open("sb1", hostPort, 3000); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := roundTrip(t, hostPort, "up"); err != nil {
		t.Fatalf("round trip before close: %v", err)
	}
	f.CloseSandbox("sb1")
	if _, err := roundTrip(t, hostPort, "gone"); err == nil {
		t.Fatal("closed sandbox's port must refuse connections")
	}
}

func TestPortForwarderSyncReconcilesMappings(t *testing.T) {
	backend := newFakeBackend(t)
	var lastGuestPort atomic.Int64
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		lastGuestPort.Store(int64(guestPort))
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backend.addr())
	}
	tc := &trackCounter{}
	f := testForwarder(t, dial, tc.track)

	keep, drop, add := freePort(t), freePort(t), freePort(t)
	if err := f.Open("sb1", keep, 3000); err != nil {
		t.Fatalf("open keep: %v", err)
	}
	if err := f.Open("sb1", drop, 8000); err != nil {
		t.Fatalf("open drop: %v", err)
	}
	if err := f.Sync("sb1", map[int]int{keep: 3000, add: 9000}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, err := roundTrip(t, keep, "keep"); err != nil {
		t.Fatalf("kept mapping must still serve: %v", err)
	}
	if _, err := roundTrip(t, drop, "drop"); err == nil {
		t.Fatal("dropped mapping must stop serving")
	}
	if _, err := roundTrip(t, add, "add"); err != nil {
		t.Fatalf("added mapping must serve: %v", err)
	}
	if lastGuestPort.Load() != 9000 {
		t.Fatalf("added mapping dialed guest port %d, want 9000", lastGuestPort.Load())
	}
}

// waitFor polls cond for up to 2 s — connection teardown is asynchronous with
// the client's close.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
