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

// portDialTimeout bounds a connection's wake + dial. A wake is normally
// sub-second (same-identity restore) but can pay a GCS-free snapshot load
// plus a 30 s agent wait on the clone path.
const portDialTimeout = 90 * time.Second

// dialGuestFunc resolves a sandbox's CURRENT guest IP — waking it first when
// hibernated — and dials guestIP:guestPort. Injected so tests can fake the
// wake path.
type dialGuestFunc func(ctx context.Context, sandboxID string, guestPort int) (net.Conn, error)

// portForwarder owns every host-port listener of the server.
type portForwarder struct {
	dial  dialGuestFunc
	track func(sandboxID string) func() // activity begin; the returned func marks done
	bind  string                        // listen address; "" = all interfaces (tests use 127.0.0.1)

	mu        sync.Mutex
	listeners map[string]map[int]*portListener // sandbox id → host port → listener
	closed    bool
}

type portListener struct {
	ln        net.Listener
	guestPort int
}

func newPortForwarder(dial dialGuestFunc, track func(string) func()) *portForwarder {
	return &portForwarder{dial: dial, track: track, listeners: map[string]map[int]*portListener{}}
}

// Open binds hostPort and starts forwarding its connections to the sandbox's
// guestPort. Idempotent for an existing identical mapping; a same-hostPort
// mapping to a different guest port is replaced.
func (f *portForwarder) Open(id string, hostPort, guestPort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	for hostPort, pl := range f.listeners[id] {
		if gp, ok := desired[hostPort]; !ok || gp != pl.guestPort {
			_ = pl.ln.Close()
			delete(f.listeners[id], hostPort)
		}
	}
	f.mu.Unlock()
	var firstErr error
	for hostPort, guestPort := range desired {
		if err := f.Open(id, hostPort, guestPort); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseSandbox releases every listener of one sandbox (destroy path). Open
// connections keep streaming until either side closes.
func (f *portForwarder) CloseSandbox(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pl := range f.listeners[id] {
		_ = pl.ln.Close()
	}
	delete(f.listeners, id)
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

// serve accepts connections on one listener until it's closed.
func (f *portForwarder) serve(id string, pl *portListener) {
	for {
		conn, err := pl.ln.Accept()
		if err != nil {
			return // listener closed (destroy/replace/shutdown)
		}
		go f.handle(id, pl.guestPort, conn)
	}
}

// handle proxies one accepted connection to the guest. The activity
// begin/done pair brackets the connection's whole lifetime, so an open
// connection pins the sandbox running exactly like an open shell does.
func (f *portForwarder) handle(id string, guestPort int, client net.Conn) {
	defer client.Close()
	done := f.track(id)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), portDialTimeout)
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
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(backend, client)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, backend)
		closeWrite(client)
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

// dialGuest is the server's dial hook for the forwarder: wake the sandbox if
// it's hibernated, then dial its CURRENT guest IP (re-read from the row that
// ensureRunning returns — a clone-path wake assigns a fresh one, so the IP
// must never be cached across the wake). Two attempts with a per-dial timeout
// cover the freeze-vs-connect race: a connection accepted while the reaper is
// mid-freeze sees a still-'running' row but a blackholed guest; the first
// dial times out and the second attempt's ensureRunning wakes it properly.
func (s *Server) dialGuest(ctx context.Context, id string, guestPort int) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		sb, err := s.ensureRunning(ctx, id)
		if err != nil {
			return nil, err
		}
		d := net.Dialer{Timeout: 10 * time.Second} // the ctx deadline still caps the total
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(sb.GuestIP, strconv.Itoa(guestPort)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// syncSandboxPorts points the forwarder at a sandbox's current mappings: the
// primary port plus every sandbox_ports row.
func (s *Server) syncSandboxPorts(ctx context.Context, sb registry.Sandbox) error {
	desired := map[int]int{sb.HostPort: s.cfg.Provisioner.Network.GuestPort}
	ports, err := s.reg.Ports(ctx, sb.ID)
	if err != nil {
		return err
	}
	for _, pm := range ports {
		desired[pm.HostPort] = pm.GuestPort
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
