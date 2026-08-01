package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

func TestConnLimitsEnforcePerSandboxAndPerHostCaps(t *testing.T) {
	l := newConnLimits(2, 3, -1) // rate limiting off; caps only

	relA1, err := l.acquire("a")
	if err != nil {
		t.Fatalf("first connection to a: %v", err)
	}
	if _, err := l.acquire("a"); err != nil {
		t.Fatalf("second connection to a: %v", err)
	}
	// Third connection to the same sandbox exceeds the per-sandbox cap even
	// though the host still has a slot free.
	if _, err := l.acquire("a"); !errors.Is(err, errSandboxConnLimit) {
		t.Fatalf("third connection to a = %v, want errSandboxConnLimit", err)
	}
	// A different sandbox is unaffected by a's cap...
	if _, err := l.acquire("b"); err != nil {
		t.Fatalf("first connection to b: %v", err)
	}
	// ...until the host-wide cap binds, which must report as such: it is the
	// limit that protects everything else on the box, creates included.
	if _, err := l.acquire("b"); !errors.Is(err, errHostConnLimit) {
		t.Fatalf("host-saturating connection = %v, want errHostConnLimit", err)
	}
	if host, sandbox := l.counts("a"); host != 3 || sandbox != 2 {
		t.Fatalf("counts = host %d, a %d; want 3 and 2", host, sandbox)
	}

	// Releasing frees capacity for both scopes, and a double release must not
	// corrupt the accounting (handlers defer release on paths that may also
	// have returned early).
	relA1()
	relA1()
	if host, sandbox := l.counts("a"); host != 2 || sandbox != 1 {
		t.Fatalf("after release counts = host %d, a %d; want 2 and 1", host, sandbox)
	}
	if _, err := l.acquire("b"); err != nil {
		t.Fatalf("connection after release: %v", err)
	}
}

func TestConnLimitsRateLimitsAcceptsAndRefills(t *testing.T) {
	l := newConnLimits(-1, -1, 10) // 10/s, burst 20, no concurrency caps
	start := time.Now()

	for i := 0; i < 20; i++ {
		if _, err := l.acquireAt("a", start); err != nil {
			t.Fatalf("burst connection %d: %v", i, err)
		}
	}
	// Burst spent: a flood of short-lived connections never accumulates against
	// the concurrency caps, so the rate limit is the control that stops it.
	if _, err := l.acquireAt("a", start); !errors.Is(err, errConnRateLimit) {
		t.Fatalf("post-burst connection = %v, want errConnRateLimit", err)
	}
	// Another sandbox has its own bucket — one noisy sandbox must not throttle
	// its neighbors.
	if _, err := l.acquireAt("b", start); err != nil {
		t.Fatalf("neighbor sandbox throttled: %v", err)
	}
	// One second of refill buys exactly the configured rate back.
	for i := 0; i < 10; i++ {
		if _, err := l.acquireAt("a", start.Add(time.Second)); err != nil {
			t.Fatalf("refilled connection %d: %v", i, err)
		}
	}
	if _, err := l.acquireAt("a", start.Add(time.Second)); !errors.Is(err, errConnRateLimit) {
		t.Fatalf("over-refill connection = %v, want errConnRateLimit", err)
	}
}

func TestConnLimitsStateDoesNotAccumulatePerSandbox(t *testing.T) {
	// The limiter is keyed by sandbox id, and ids are never reused — an entry
	// per id ever seen would be an unbounded map on a long-lived server.
	l := newConnLimits(4, 8, -1)
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("sb-%d", i)
		release, err := l.acquire(id)
		if err != nil {
			t.Fatalf("acquire %s: %v", id, err)
		}
		release()
	}
	l.mu.Lock()
	n := len(l.byID)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("limiter retained state for %d idle sandboxes, want 0", n)
	}

	// forget() is the destroy path: it drops the entry outright.
	release, err := l.acquire("gone")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	l.forget("gone")
	l.mu.Lock()
	_, present := l.byID["gone"]
	l.mu.Unlock()
	if present {
		t.Fatalf("forget() left limiter state behind")
	}
}

func TestForwarderRefusesConnectionsPastPerSandboxCap(t *testing.T) {
	backend := newFakeBackend(t)
	var dials atomic.Int64
	dial := func(ctx context.Context, id string, guestPort int) (net.Conn, error) {
		dials.Add(1)
		return net.Dial("tcp", backend.addr())
	}
	tc := &trackCounter{}
	// One connection per sandbox, so the second must be refused at accept.
	f := testForwarderWithLimits(t, dial, tc.track, newConnLimits(1, 8, -1))

	hostPort := freePort(t)
	if err := f.Open("sb", hostPort, 3000); err != nil {
		t.Fatalf("open listener: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()
	// Drive a round trip so the connection is definitely admitted and open.
	fmt.Fprintln(first, "hello")
	if line, err := bufio.NewReader(first).ReadString('\n'); err != nil || line != "echo:hello\n" {
		t.Fatalf("first connection round trip = %q, %v", line, err)
	}

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("second dial: %v", err) // the listener accepts, then refuses
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("refused connection stayed open and read %d bytes", n)
	}

	// The refusal must happen before the guest is dialed and before an activity
	// pin is taken: the point of the cap is that an over-limit connection costs
	// the host (and the sandbox's hibernation clock) nothing.
	if got := dials.Load(); got != 1 {
		t.Fatalf("guest dialed %d times, want 1 (the refused connection must not dial)", got)
	}
	if got := tc.begun.Load(); got != 1 {
		t.Fatalf("activity pinned %d times, want 1", got)
	}

	// Once the admitted connection closes, capacity returns.
	first.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if host, sandbox := f.limits.counts("sb"); host == 0 && sandbox == 0 {
			break
		}
		if time.Now().After(deadline) {
			host, sandbox := f.limits.counts("sb")
			t.Fatalf("release did not happen: host %d, sandbox %d open", host, sandbox)
		}
		time.Sleep(10 * time.Millisecond)
	}
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("third dial: %v", err)
	}
	defer third.Close()
	fmt.Fprintln(third, "again")
	if line, err := bufio.NewReader(third).ReadString('\n'); err != nil || line != "echo:again\n" {
		t.Fatalf("post-release round trip = %q, %v", line, err)
	}
}

func TestConnectTunnelRefusedOverLimitBeforeRegistryReads(t *testing.T) {
	s := connectTestServer(t)
	if _, err := s.reg.AddURLPort(context.Background(), "sandbox-id", 3000); err != nil {
		t.Fatal(err)
	}
	// Saturate the host cap so the tunnel handler cannot admit.
	s.pf.limits = newConnLimits(1, 1, -1)
	if _, err := s.pf.limits.acquire("other"); err != nil {
		t.Fatalf("prime limiter: %v", err)
	}
	dialed := false
	s.pf.dial = func(context.Context, string, int) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not dial")
	}

	beforeRoutes, beforePorts := s.reg.PortReadCounts()
	req := httptest.NewRequest(http.MethodConnect, "/sandboxes/sandbox-id/connect/3000", nil)
	req.SetPathValue("id", "sandbox-id")
	req.SetPathValue("port", "3000")
	w := httptest.NewRecorder()
	s.handleConnectPort(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body %s", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After: the edge has nothing to back off on")
	}
	if dialed {
		t.Fatal("refused tunnel dialed the guest")
	}
	// Admission runs before the exposure lookups, so a flood cannot use the
	// public data path to hammer the registry.
	afterRoutes, afterPorts := s.reg.PortReadCounts()
	if afterRoutes != beforeRoutes || afterPorts != beforePorts {
		t.Fatalf("refused tunnel still read port mappings (%d→%d)", beforePorts, afterPorts)
	}
}

func TestPortDialBudgetShortensWhenNoWakeIsNeeded(t *testing.T) {
	s, reg := capacityTestServer(t)
	ctx := context.Background()
	if _, err := reg.Create(ctx, "sb", "", "/tmp/sb.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A running sandbox needs a TCP handshake on the local bridge, not the
	// clone-path wake allowance.
	if got := s.portDialBudget(ctx, "sb"); got != portDialRunningTimeout {
		t.Fatalf("running sandbox budget = %s, want %s", got, portDialRunningTimeout)
	}
	if err := reg.Hibernate(ctx, "sb"); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if got := s.portDialBudget(ctx, "sb"); got != portDialWakeTimeout {
		t.Fatalf("hibernated sandbox budget = %s, want %s", got, portDialWakeTimeout)
	}
	// Unknown ids and read failures fall back to the generous side: the budget
	// only sizes a timeout, and the error surfaces from the resolve itself.
	if got := s.portDialBudget(ctx, "missing"); got != portDialWakeTimeout {
		t.Fatalf("unknown sandbox budget = %s, want %s", got, portDialWakeTimeout)
	}
}

func TestResolveRunningCoalescesConcurrentLookups(t *testing.T) {
	f := testForwarder(t, nil, func(string) func() { return func() {} })

	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	resolve := func(ctx context.Context) (registry.Sandbox, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return registry.Sandbox{ID: "sb", GuestIP: "172.16.0.10", Status: registry.StatusRunning}, nil
	}

	const waiters = 16
	results := make(chan registry.Sandbox, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			sb, err := f.resolveRunning(context.Background(), "sb", resolve)
			if err != nil {
				t.Errorf("resolveRunning: %v", err)
			}
			results <- sb
		}()
	}
	<-started
	// Give the stragglers time to join the in-flight lookup rather than start
	// their own.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < waiters; i++ {
		select {
		case sb := <-results:
			if sb.GuestIP != "172.16.0.10" {
				t.Fatalf("resolved IP = %q", sb.GuestIP)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("waiter never resolved")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d lookups for %d concurrent connections, want 1", got, waiters)
	}

	// The flight is retired once it completes, so the NEXT connection resolves
	// afresh. This is what keeps the guest IP from being cached across a wake:
	// a hibernated sandbox releases its tap and IP to the pools, so a stale IP
	// could belong to a different sandbox entirely.
	release2 := make(chan struct{})
	close(release2)
	if _, err := f.resolveRunning(context.Background(), "sb", resolve); err != nil {
		t.Fatalf("post-flight resolve: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("post-flight lookups = %d, want 2 (the value must not be cached)", got)
	}
}

func TestResolveRunningSurvivesLeaderCancellation(t *testing.T) {
	f := testForwarder(t, nil, func(string) func() { return func() {} })

	proceed := make(chan struct{})
	resolve := func(ctx context.Context) (registry.Sandbox, error) {
		<-proceed
		// The leader's own context is gone by now; the flight must not have
		// inherited its cancellation, or one client hanging up would fail the
		// wake for every other connection waiting on it.
		if err := ctx.Err(); err != nil {
			return registry.Sandbox{}, err
		}
		return registry.Sandbox{ID: "sb", GuestIP: "172.16.0.11", Status: registry.StatusRunning}, nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := f.resolveRunning(leaderCtx, "sb", resolve)
		leaderDone <- err
	}()
	// Wait until the flight exists, then abandon the leader.
	for {
		f.mu.Lock()
		_, ok := f.flights["sb"]
		f.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	joinerDone := make(chan registry.Sandbox, 1)
	joinerErr := make(chan error, 1)
	go func() {
		sb, err := f.resolveRunning(context.Background(), "sb", resolve)
		joinerDone <- sb
		joinerErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned leader err = %v, want context.Canceled", err)
	}
	close(proceed)

	select {
	case err := <-joinerErr:
		if err != nil {
			t.Fatalf("joiner inherited the leader's cancellation: %v", err)
		}
		if sb := <-joinerDone; sb.GuestIP != "172.16.0.11" {
			t.Fatalf("joiner resolved %q", sb.GuestIP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("joiner never resolved after the leader hung up")
	}
}

// io is used by the shared fakeBackend helpers in portproxy_test.go; keep the
// import referenced so this file compiles standalone under -run filters.
var _ = io.Discard
