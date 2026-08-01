package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Forwarded host ports used to be kernel DNAT rules, which made the traffic
// invisible to the server: it couldn't reset the idle-hibernation clock (a
// sandbox actively serving HTTP got frozen mid-use), and a connection to a
// hibernated sandbox's port hung forever. portForwarder replaces DNAT with
// in-process TCP listeners: every accepted connection counts as activity and
// pins the sandbox running while it's open, a connection to a hibernated
// sandbox wakes it first, and the guest IP is re-read after the wake (a
// clone-path wake changes it). Listeners live from create to destroy —
// hibernation deliberately keeps them bound, which is what makes
// wake-on-connect work (and why a hibernated sandbox keeps its host ports
// hard-reserved in the registry).

// Dial budgets. The 90 s ceiling exists for ONE case: a clone-path wake, which
// can pay a snapshot load plus a 30 s agent wait. Applying it to every
// connection meant an already-running sandbox whose guest port simply isn't
// answering held a goroutine, an activity pin (which defeats hibernation) and a
// listener slot for a minute and a half. The budget is therefore chosen per
// attempt from the sandbox's current status: a running sandbox needs a TCP
// handshake on the local bridge, nothing more. The wake budget is still
// available on the retry — a sandbox found running can be frozen a moment
// later (see dialGuest's two-attempt loop), and that attempt re-derives its own
// budget instead of inheriting the short one.
const (
	portDialWakeTimeout    = 90 * time.Second
	portDialRunningTimeout = 20 * time.Second
)

// Data-plane fan-in limits.
//
// Everything below protects the host from its own data plane. A forwarded host
// port is reachable by anyone who can reach the worker, and a CONNECT tunnel by
// the public edge on behalf of the internet; both used to accept without any
// bound. Every accepted connection costs a goroutine, a 90 s dial budget, an
// activity pin, and registry reads, so one client could hold tens of thousands
// of them and starve the thing that actually matters on the host — creates. The
// edge already has exactly these two controls (a MaxConnections semaphore and a
// per-source rate limiter); the worker it dials had neither, which is the wrong
// way round: the edge can be scaled out, the worker's sandboxes cannot.
//
// The limits are ACCEPT-side and fail fast (close the connection / 429) rather
// than queueing. Queueing behind the dial budget converts a goroutine flood
// into a memory flood and delays the failure the client needs to see; a refused
// TCP connection is a signal every client already understands.
//
// Defaults are deliberately far above any well-behaved workload: a browser or
// an SSH session opens single-digit connections, a busy dev server tens. They
// bite only on abuse or a runaway client.
const (
	defaultMaxConnsPerSandbox = 256
	defaultMaxConnsTotal      = 4096
	defaultConnRatePerSec     = 200 // per sandbox; burst is 2× this
)

var (
	errSandboxConnLimit = errors.New("too many concurrent connections to this sandbox")
	errHostConnLimit    = errors.New("too many concurrent forwarded connections on this host")
	errConnRateLimit    = errors.New("connection rate limit exceeded for this sandbox")
)

// isConnLimitError reports whether err came from the accept limiter, so HTTP
// callers can answer 429 (retry later) instead of 500 (broken).
func isConnLimitError(err error) bool {
	return errors.Is(err, errSandboxConnLimit) || errors.Is(err, errHostConnLimit) ||
		errors.Is(err, errConnRateLimit)
}

// connLimits caps concurrent data-plane connections per sandbox and per host,
// and rate-limits accepts per sandbox. Any field <= 0 disables that one check.
type connLimits struct {
	perSandbox int
	total      int
	rate       float64 // accepts per second per sandbox
	burst      float64

	mu   sync.Mutex
	open int // host-wide
	byID map[string]*sandboxConns
}

// sandboxConns is one sandbox's slice of the limiter: its open count, its token
// bucket, and throttled-logging state (a rejection storm must not become a log
// storm — that just moves the resource exhaustion to the disk).
type sandboxConns struct {
	open    int
	tokens  float64
	refill  time.Time
	dropped int
	lastLog time.Time
}

func newConnLimits(perSandbox, total, ratePerSec int) *connLimits {
	if perSandbox == 0 {
		perSandbox = defaultMaxConnsPerSandbox
	}
	if total == 0 {
		total = defaultMaxConnsTotal
	}
	if ratePerSec == 0 {
		ratePerSec = defaultConnRatePerSec
	}
	l := &connLimits{perSandbox: perSandbox, total: total, byID: map[string]*sandboxConns{}}
	if ratePerSec > 0 {
		l.rate = float64(ratePerSec)
		l.burst = 2 * float64(ratePerSec)
	}
	return l
}

// acquire admits one connection for a sandbox, returning the release func to
// call when it closes. A non-nil error means the connection must be refused.
func (l *connLimits) acquire(id string) (func(), error) { return l.acquireAt(id, time.Now()) }

// acquireAt is acquire with an injected clock (tests drive the token bucket).
func (l *connLimits) acquireAt(id string, now time.Time) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.byID[id]
	if st == nil {
		st = &sandboxConns{tokens: l.burst, refill: now}
		l.byID[id] = st
	}
	// Host cap first: it is the one that protects everything else on the box.
	if l.total > 0 && l.open >= l.total {
		return nil, l.rejectLocked(id, st, now, errHostConnLimit)
	}
	if l.perSandbox > 0 && st.open >= l.perSandbox {
		return nil, l.rejectLocked(id, st, now, errSandboxConnLimit)
	}
	if l.rate > 0 {
		l.refillLocked(st, now)
		if st.tokens < 1 {
			return nil, l.rejectLocked(id, st, now, errConnRateLimit)
		}
		st.tokens--
	}
	l.open++
	st.open++
	var once sync.Once
	return func() { once.Do(func() { l.release(id) }) }, nil
}

func (l *connLimits) refillLocked(st *sandboxConns, now time.Time) {
	if elapsed := now.Sub(st.refill); elapsed > 0 {
		st.tokens += elapsed.Seconds() * l.rate
		if st.tokens > l.burst {
			st.tokens = l.burst
		}
		st.refill = now
	}
}

func (l *connLimits) rejectLocked(id string, st *sandboxConns, now time.Time, err error) error {
	st.dropped++
	if now.Sub(st.lastLog) >= time.Second {
		fmt.Fprintf(os.Stderr, "[%s] port proxy: refused connection: %v (%d refused since last report, %d open on sandbox, %d on host)\n",
			id, err, st.dropped, st.open, l.open)
		st.lastLog, st.dropped = now, 0
	}
	return err
}

func (l *connLimits) release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.byID[id]
	if st == nil {
		return
	}
	if st.open > 0 {
		st.open--
		l.open--
	}
	// Drop the entry once the sandbox is idle and its bucket has fully
	// refilled, so the map can't accumulate one entry per id ever seen. Live
	// sandboxes bound it anyway (forget() runs on destroy), but ids of
	// destroyed sandboxes must not linger.
	l.refillLocked(st, time.Now())
	if st.open == 0 && st.dropped == 0 && (l.rate <= 0 || st.tokens >= l.burst) {
		delete(l.byID, id)
	}
}

// forget drops a destroyed sandbox's limiter state. Open connections keep their
// host-wide accounting (their release still decrements l.open) — only the
// per-sandbox entry goes.
func (l *connLimits) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.byID[id]; st != nil && st.open == 0 {
		delete(l.byID, id)
	}
}

// counts reports the host-wide and per-sandbox open connection counts (tests,
// and cheap enough for future metrics).
func (l *connLimits) counts(id string) (host, sandbox int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.byID[id]; st != nil {
		sandbox = st.open
	}
	return l.open, sandbox
}

// dialGuestFunc resolves a sandbox's CURRENT guest IP — waking it first when
// hibernated — and dials guestIP:guestPort. Injected so tests can fake the
// wake path.
type dialGuestFunc func(ctx context.Context, sandboxID string, guestPort int) (net.Conn, error)

// portForwarder owns every host-port listener of the server.
type portForwarder struct {
	dial   dialGuestFunc
	track  func(sandboxID string) func() // activity begin; the returned func marks done
	limits *connLimits                   // accept-side fan-in caps; shared with the CONNECT path
	bind   string                        // listen address; "" = all interfaces (tests use 127.0.0.1)

	mu        sync.Mutex
	listeners map[string]map[int]*portListener // sandbox id → host port → listener
	closed    bool

	// resolve coalesces concurrent "which IP is this sandbox on" lookups —
	// see resolveRunning.
	flights map[string]*resolveFlight
}

type portListener struct {
	ln        net.Listener
	guestPort int
}

func newPortForwarder(dial dialGuestFunc, track func(string) func(), limits *connLimits) *portForwarder {
	if limits == nil {
		limits = newConnLimits(0, 0, 0)
	}
	return &portForwarder{
		dial:      dial,
		track:     track,
		limits:    limits,
		listeners: map[string]map[int]*portListener{},
		flights:   map[string]*resolveFlight{},
	}
}

// Open binds hostPort and starts forwarding its connections to the sandbox's
// guestPort. Idempotent for an existing identical mapping; a same-hostPort
// mapping to a different guest port is replaced.
func (f *portForwarder) Open(id string, hostPort, guestPort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openLocked(id, hostPort, guestPort)
}

// openLocked is Open's body; f.mu must be held. Sync uses it to reconcile a
// sandbox's whole listener set inside ONE critical section — releasing the lock
// between "close stale" and "open desired" let a concurrent CloseSandbox land
// in the middle, so the sandbox ended up with a half-reconciled set (and, when
// the destroy went first, listeners bound for a row that no longer exists,
// permanently leaking those host ports from the pool's point of view).
func (f *portForwarder) openLocked(id string, hostPort, guestPort int) error {
	if f.closed {
		return errors.New("port forwarder is shut down")
	}
	if pl, ok := f.listeners[id][hostPort]; ok {
		if pl.guestPort == guestPort {
			return nil // already forwarding — e.g. a wake, whose listener persisted
		}
		_ = pl.ln.Close()
		delete(f.listeners[id], hostPort)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(f.bind, strconv.Itoa(hostPort)))
	if err != nil {
		return fmt.Errorf("listen on host port %d: %w", hostPort, err)
	}
	if f.listeners[id] == nil {
		f.listeners[id] = map[int]*portListener{}
	}
	pl := &portListener{ln: ln, guestPort: guestPort}
	f.listeners[id][hostPort] = pl
	go f.serve(id, pl)
	return nil
}

// Sync reconciles a sandbox's listeners to exactly the desired
// hostPort → guestPort set: stale ones close, missing ones open. Used at
// startup (re-binding a hibernated sandbox's ports so wake-on-connect
// survives a server restart) and defensively after a wake.
func (f *portForwarder) Sync(id string, desired map[int]int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for hostPort, pl := range f.listeners[id] {
		if gp, ok := desired[hostPort]; !ok || gp != pl.guestPort {
			_ = pl.ln.Close()
			delete(f.listeners[id], hostPort)
		}
	}
	var firstErr error
	for hostPort, guestPort := range desired {
		if err := f.openLocked(id, hostPort, guestPort); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseSandbox releases every listener of one sandbox (destroy path). Open
// connections keep streaming until either side closes.
func (f *portForwarder) CloseSandbox(id string) {
	f.mu.Lock()
	for _, pl := range f.listeners[id] {
		_ = pl.ln.Close()
	}
	delete(f.listeners, id)
	f.mu.Unlock()
	f.limits.forget(id)
}

// CloseAll releases every listener (server shutdown).
func (f *portForwarder) CloseAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for _, m := range f.listeners {
		for _, pl := range m {
			_ = pl.ln.Close()
		}
	}
	f.listeners = map[string]map[int]*portListener{}
}

// serve accepts connections on one listener until it's closed. Admission runs
// HERE, before the per-connection goroutine exists: taking the limit inside
// f.handle would still pay a goroutine (and its stack) per attempt, which is
// most of what an accept flood costs.
func (f *portForwarder) serve(id string, pl *portListener) {
	for {
		conn, err := pl.ln.Accept()
		if err != nil {
			return // listener closed (destroy/replace/shutdown)
		}
		release, err := f.limits.acquire(id)
		if err != nil {
			conn.Close() // the limiter already logged, throttled
			continue
		}
		go f.handle(id, pl.guestPort, conn, release)
	}
}

// handle proxies one accepted connection to the guest. The activity
// begin/done pair brackets the connection's whole lifetime, so an open
// connection pins the sandbox running exactly like an open shell does.
func (f *portForwarder) handle(id string, guestPort int, client net.Conn, release func()) {
	defer client.Close()
	defer release()
	done := f.track(id)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), portDialWakeTimeout)
	backend, err := f.dial(ctx, id, guestPort)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] port proxy: dial guest port %d: %v\n", id, guestPort, err)
		return
	}
	defer backend.Close()

	// Bidirectional copy with TCP half-close semantics: when one direction
	// EOFs, shut down only the write side of its peer so the other direction
	// can finish (e.g. a client that closes its request stream and then reads
	// the response).
	pipeConns(client, backend)
}

// pipeConns copies bytes in both directions and preserves TCP half-close
// semantics. The worker CONNECT endpoint reuses this exact data-plane path.
func pipeConns(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	wg.Wait()
}

// closeWrite half-closes a connection's write side when supported (TCP),
// falling back to a full close.
func closeWrite(c net.Conn) {
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = c.Close()
}

// resolveFlight is one in-flight "resolve this sandbox to a running row" call
// that later arrivals join instead of issuing their own.
type resolveFlight struct {
	done chan struct{}
	sb   registry.Sandbox
	err  error
}

// resolveRunning resolves a sandbox to a running row, coalescing concurrent
// calls for the same id onto one.
//
// This is the ONLY form of caching that is safe on this path, and the reason
// matters: a hibernated sandbox RELEASES its tap and IP back to the pools, so a
// guest IP cached past a freeze can already belong to a DIFFERENT sandbox. A
// stale IP here is therefore not a slow dial, it is a cross-sandbox misroute —
// which is why the rule is "never cache the IP across a wake", and why a plain
// TTL cache (any TTL) is unshippable no matter how short. Coalescing sidesteps
// that entirely: the shared value is at most one in-flight lookup old, exactly
// the window that already exists between any resolve and its own dial, and a
// joiner can never observe a value from before a completed transition (once the
// leader returns, the flight is retired and the next caller starts a fresh one).
//
// It also collapses the wake herd: ensureRunning serializes same-id callers on
// the wake lock, so N connections arriving on a hibernated sandbox used to
// queue N deep behind one restore and then each issue their own registry read.
//
// The flight runs on a context detached from the leader's, bounded by the wake
// budget: otherwise a leader whose client hangs up cancels the wake underneath
// every joiner. Each caller still honors its own ctx while waiting.
func (f *portForwarder) resolveRunning(ctx context.Context, id string, resolve func(context.Context) (registry.Sandbox, error)) (registry.Sandbox, error) {
	f.mu.Lock()
	fl, joined := f.flights[id]
	if !joined {
		fl = &resolveFlight{done: make(chan struct{})}
		f.flights[id] = fl
	}
	f.mu.Unlock()

	if !joined {
		go func() {
			flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), portDialWakeTimeout)
			defer cancel()
			fl.sb, fl.err = resolve(flightCtx)
			f.mu.Lock()
			delete(f.flights, id)
			f.mu.Unlock()
			close(fl.done)
		}()
	}

	select {
	case <-fl.done:
		return fl.sb, fl.err
	case <-ctx.Done():
		return registry.Sandbox{}, ctx.Err()
	}
}

// dialGuest is the server's dial hook for the forwarder: wake the sandbox if
// it's hibernated, then dial its CURRENT guest IP (re-read from the row that
// ensureRunning returns — a clone-path wake assigns a fresh one, so the IP
// must never be cached across the wake). Two attempts with a per-dial timeout
// cover the freeze-vs-connect race: a connection accepted while the reaper is
// mid-freeze sees a still-'running' row but a blackholed guest; the first
// dial times out and the second attempt's ensureRunning wakes it properly.
//
// Each attempt derives its own budget from the sandbox's status (see the
// portDial* constants), so the 90 s wake allowance is spent only when a wake is
// actually in play — including on attempt 2, which is where the freeze race
// lands. ctx from the caller remains the hard ceiling.
func (s *Server) dialGuest(ctx context.Context, id string, guestPort int) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err, resolveFailed := s.dialGuestOnce(ctx, id, guestPort)
		if err == nil {
			return conn, nil
		}
		if resolveFailed {
			// Unchanged from the original loop: an unknown sandbox stays
			// unknown and a capacity-rejected wake must surface as its own
			// error (404/503), not be retried into a generic dial failure.
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// dialGuestOnce is one attempt. The third result distinguishes "couldn't
// resolve/wake the sandbox" (final) from "couldn't reach the guest" (retryable).
func (s *Server) dialGuestOnce(ctx context.Context, id string, guestPort int) (net.Conn, error, bool) {
	ctx, cancel := context.WithTimeout(ctx, s.portDialBudget(ctx, id))
	defer cancel()
	sb, err := s.pf.resolveRunning(ctx, id, func(fctx context.Context) (registry.Sandbox, error) {
		return s.ensureRunning(fctx, id)
	})
	if err != nil {
		return nil, err, true
	}
	d := net.Dialer{Timeout: 10 * time.Second} // the ctx deadline still caps the total
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(sb.GuestIP, strconv.Itoa(guestPort)))
	return conn, err, false
}

// portDialBudget picks a connection attempt's total budget: the short one when
// the sandbox is already running (all that's left is a TCP handshake over the
// local bridge), the wake allowance otherwise. A read failure is treated as
// "might need a wake" — this only sizes a timeout, so the conservative side is
// the generous one. The read is a single indexed lookup on the registry's
// read-only handle, so it does not queue behind creates.
func (s *Server) portDialBudget(ctx context.Context, id string) time.Duration {
	sb, err := s.reg.Get(ctx, id)
	if err != nil || sb.Status != registry.StatusRunning {
		return portDialWakeTimeout
	}
	return portDialRunningTimeout
}

// syncSandboxPorts points the forwarder at all explicitly exposed mappings.
func (s *Server) syncSandboxPorts(ctx context.Context, sb registry.Sandbox) error {
	desired := map[int]int{}
	ports, err := s.reg.Ports(ctx, sb.ID)
	if err != nil {
		return err
	}
	for _, pm := range ports {
		if pm.HostPort != 0 {
			desired[pm.HostPort] = pm.GuestPort
		}
	}
	return s.pf.Sync(sb.ID, desired)
}

// reopenPortListeners re-binds the port-forward listeners of every routed
// sandbox at startup. reconcile() has already destroyed all stale running
// rows, so this effectively covers hibernated sandboxes — whose
// wake-on-connect contract requires their host ports to be listening even
// though no VM runs.
func (s *Server) reopenPortListeners(ctx context.Context) {
	rows, err := s.reg.ListRouted(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reopen port listeners: list sandboxes: %v\n", err)
		return
	}
	for _, sb := range rows {
		if err := s.syncSandboxPorts(ctx, sb); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] reopen port listeners: %v\n", sb.ID, err)
		}
	}
}
