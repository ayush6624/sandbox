package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// agentTransport is the host→guest transport, shared by every sandbox on this
// worker. It exists because http.DefaultTransport is sized for a browser-ish
// workload and this is the opposite shape: every guest is a distinct "host"
// (its own bridge IP), and one worker fronts hundreds of them.
//
// DefaultTransport's MaxIdleConnsPerHost is 2, so the third concurrent
// exec/file request to a single sandbox — trivially reached by a client running
// commands in parallel — could not reuse a connection and paid a fresh TCP
// handshake, leaving TIME_WAIT behind. Its MaxIdleConns is 100, so past ~100
// sandboxes per host the pool evicted idle connections continuously and
// essentially every request handshaked. internal/client made exactly this fix
// for the gateway→worker path; the numbers here are sized per SANDBOX rather
// than per host: MaxIdleConnsPerHost covers a sandbox's parallel calls, and
// MaxIdleConns is generous enough that a full host never evicts as a side
// effect of its own breadth.
//
// IdleConnTimeout is deliberately shorter than the gateway's 90 s: guests are
// destroyed and hibernated constantly, and an idle connection to a guest that
// no longer exists is a wasted file descriptor on the host.
//
// The pool is keyed on the SANDBOX, not on the guest IP — see agentAuthority.
var agentTransport = &http.Transport{
	MaxIdleConns:        4096,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     60 * time.Second,
	// Guest IPs live on the local bridge: an inherited HTTP(S)_PROXY from the
	// environment (which DefaultTransport honors) would be wrong for every one
	// of them, and Proxy lookups are per-request work for nothing.
	Proxy:       nil,
	DialContext: dialAgentAuthority,
	// The agent is plain HTTP/1.1; skip the h2 negotiation attempt.
	ForceAttemptHTTP2: false,
}

// guestOneShotTransport serves the once-per-lifecycle guest calls (/identity,
// /clock, /ssh-key, /snapshot-poll, /health). Keep-alive is DISABLED: each
// fires once per VM bring-up, so pooling buys nothing, and a pooled connection
// here is actively dangerous for the reason described on agentAuthority — these
// calls run at the exact moment a recycled guest IP has just changed owner.
// They previously rode http.DefaultTransport, whose pool is keyed on the IP
// alone.
var guestOneShotTransport = &http.Transport{
	DisableKeepAlives: true,
	Proxy:             nil,
	DialContext:       agentDialer.DialContext,
	ForceAttemptHTTP2: false,
}

var agentDialer = &net.Dialer{
	Timeout:   5 * time.Second, // local bridge; a slow connect means a dead guest
	KeepAlive: 30 * time.Second,
}

// agentAuthority builds the URL authority used for BOTH the connection-pool key
// and the dial. It deliberately encodes the sandbox id alongside the guest IP.
//
// Guest IPs are drawn from a small per-host pool and are RECYCLED the instant a
// sandbox is destroyed or hibernated (their partial unique indexes only bind
// running rows). net/http keys its idle-connection pool on the URL authority,
// so an IP-only authority lets a brand-new sandbox inherit a live keep-alive
// connection to the DEAD VM that previously held that address. The dead peer
// RSTs it — and net/http will NOT silently retry a POST/PUT carrying a body, so
// this surfaced to users as "502 agent unreachable: read: connection reset by
// peer" on exec and file writes under churn, while GETs (which are retried)
// looked fine. Including the id makes a recycled address a different pool key,
// so the stale entry is never reused; it also covers a clone-path wake, which
// keeps the sandbox id but moves it to a NEW IP.
//
// The synthetic authority never reaches DNS: dialAgentAuthority parses the real
// address back out, and callers set req.Host so the wire Host header stays a
// plain ip:port. Sandbox ids are UUIDs and IPv4 literals contain no '_', so the
// last '_' is an unambiguous separator.
func agentAuthority(sandboxID, guestIP string) string {
	return fmt.Sprintf("%s_%s:%d", sandboxID, guestIP, agentapi.Port)
}

// agentHostPort is the real address an agentAuthority stands for — the value
// callers put in req.Host so the guest sees an ordinary Host header.
func agentHostPort(guestIP string) string {
	return fmt.Sprintf("%s:%d", guestIP, agentapi.Port)
}

// dialAgentAuthority resolves the synthetic authority built by agentAuthority
// back to the guest address and dials it. An address without the separator is
// dialed unchanged, so a plain ip:port target still works.
func dialAgentAuthority(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if i := strings.LastIndex(host, "_"); i >= 0 {
		host = host[i+1:]
	}
	return agentDialer.DialContext(ctx, network, net.JoinHostPort(host, port))
}

// agentClient talks to in-guest sandboxd agents. No overall timeout — exec
// requests are bounded by their own timeout_sec and the request context.
var agentClient = &http.Client{Transport: agentTransport}

// handleAgentProxy forwards a request to the sandbox's in-guest agent,
// rewriting /sandboxes/{id}/<endpoint> to http://guestIP:agentPort/<endpoint>.
// A hibernated sandbox is woken first, so callers never see the freeze; the
// begin/done pair marks the sandbox busy (and its idle clock reset) for the
// whole request, including long-running exec streams.
func (s *Server) handleAgentProxy(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Validate before tracking so bogus ids do not leak activity entries.
		if _, err := s.reg.Get(r.Context(), id); err != nil {
			capacityOrHTTPError(w, statusFor(err), err)
			return
		}
		// Pin before ensureRunning takes the lifecycle lock. If hibernation
		// already passed its busy check, ensureRunning waits for it and wakes
		// the sandbox; otherwise the new pin makes the reaper back off.
		done := s.act.begin(id)
		defer done()
		sb, err := s.ensureRunning(r.Context(), id)
		if err != nil {
			// A capacity-rejected wake (memory budget/pool) surfaces as 503 +
			// Retry-After: the sandbox stays hibernated and wakeable later.
			capacityOrHTTPError(w, statusFor(err), err)
			return
		}

		// Authority carries the sandbox id so the connection pool can never hand
		// this request a keep-alive connection belonging to a previous owner of
		// a recycled guest IP (see agentAuthority).
		url := fmt.Sprintf("http://%s/%s", agentAuthority(id, sb.GuestIP), endpoint)
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
		if err != nil {
			httpError(w, 500, err)
			return
		}
		// The guest sees an ordinary ip:port Host, not the pool-keying authority.
		req.Host = agentHostPort(sb.GuestIP)
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))

		resp, err := agentClient.Do(req)
		if err != nil {
			httpError(w, 502, fmt.Errorf("agent unreachable: %w", err))
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		// Flush as the agent produces data so streamed responses
		// (e.g. /exec/stream NDJSON) reach the client immediately.
		var out io.Writer = w
		if f, ok := w.(http.Flusher); ok {
			out = flushWriter{w: w, f: f}
		}
		_, _ = io.Copy(out, resp.Body)
	}
}

// handleShellProxy reverse-proxies the /shell WebSocket to the sandbox's
// in-guest agent. httputil.ReverseProxy transparently handles the Upgrade
// handshake and then streams raw bytes both ways, so the interactive pty works
// over either the Unix socket or the bearer-auth'd TCP listener unchanged.
// Errors before the proxy takes over (unknown id, failed wake, unreachable
// agent) are delivered as WebSocket close frames (4404/4500/4502) when the
// request is an upgrade, so browser clients see the reason, not a bare 1006.
func (s *Server) handleShellProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.reg.Get(r.Context(), id); err != nil {
			shellError(w, r, statusFor(err), err)
			return
		}
		// An open shell pins the sandbox before lifecycle reconciliation and
		// for its whole lifetime (ServeHTTP returns when the socket closes).
		done := s.act.begin(id)
		defer done()
		sb, err := s.ensureRunning(r.Context(), id)
		if err != nil {
			shellError(w, r, statusFor(err), err)
			return
		}
		target := &url.URL{Scheme: "http", Host: agentAuthority(id, sb.GuestIP)}
		proxy := httputil.NewSingleHostReverseProxy(target)
		// Same sandbox-keyed pool as the REST path; without an explicit
		// transport this would fall back to http.DefaultTransport, whose pool is
		// keyed on the guest IP alone.
		proxy.Transport = agentTransport
		// NewSingleHostReverseProxy joins paths; rewrite to the agent's /shell
		// (the incoming path is /sandboxes/{id}/shell) while preserving the
		// cols/rows/cwd query string. access_token is auth plumbing for
		// browser WebSockets (see bearerAuth) — don't leak it into the guest.
		base := proxy.Director
		proxy.Director = func(req *http.Request) {
			base(req)
			req.URL.Path = "/shell"
			// Director runs per attempt; keep the wire Host a plain ip:port.
			req.Host = agentHostPort(sb.GuestIP)
			if q := req.URL.Query(); q.Has("access_token") {
				q.Del("access_token")
				req.URL.RawQuery = q.Encode()
			}
			// bearerAuth already consumed the subprotocol credential; the guest
			// must never see it.
			wsutil.StripBearerSubprotocol(req)
		}
		// The guest agent doesn't negotiate subprotocols, so this hop completes
		// the negotiation the client opened — omitting it makes the client fail
		// the connection with an opaque 1006.
		proto := wsutil.NegotiatedSubprotocol(r)
		proxy.ModifyResponse = func(resp *http.Response) error {
			wsutil.EchoSubprotocol(resp, proto)
			return nil
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			shellError(w, r, http.StatusBadGateway, fmt.Errorf("agent unreachable: %w", err))
		}
		proxy.ServeHTTP(w, r)
	}
}

// shellError reports a shell-endpoint failure: as a post-handshake WebSocket
// close frame (code 4000+status) when the request is an upgrade — the only
// form browsers surface to the page — falling back to a plain HTTP error.
func shellError(w http.ResponseWriter, r *http.Request, status int, err error) {
	if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseCodeFor(status), err.Error()) == nil {
		return
	}
	httpError(w, status, err)
}

// flushWriter flushes the ResponseWriter after every write.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		fw.f.Flush()
	}
	return n, err
}

// syncGuestClock steps the guest's wall clock to the host's via the agent's
// POST /clock. Every snapshot resume (hot create, fan-out, 1:1 restore,
// hibernation wake) leaves the guest's CLOCK_REALTIME frozen at
// snapshot-creation time. The MMDS epoch_ms push covers this too, but the
// thaw agent polls MMDS on a 200ms tick that can lag the /health readiness
// gate — this call, made after waitForAgent, makes the step deterministic
// before the sandbox is handed out. Best-effort by design: an old baked agent
// without /clock answers 404 (log, never fail the resume — the MMDS poll
// still steps agents new enough to know epoch_ms).
func syncGuestClock(ctx context.Context, guestIP string) {
	body, _ := json.Marshal(agentapi.ClockSyncRequest{UnixNano: time.Now().UnixNano()})
	url := fmt.Sprintf("http://%s:%d/clock", guestIP, agentapi.Port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second, Transport: guestOneShotTransport}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clock sync %s: %v\n", guestIP, err)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "clock sync %s: agent has no /clock (old sandboxd — re-run install-agent)\n", guestIP)
	case resp.StatusCode >= 400:
		msg, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "clock sync %s: HTTP %d: %s\n", guestIP, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// initializeGuestIdentity asks sandboxd to rotate identity inherited from the
// base image or snapshot. It is mandatory for every independent create and
// idempotent for retries with the same sandbox ID. Pause/resume deliberately
// does not call it because that lifecycle preserves sandbox identity.
func initializeGuestIdentity(ctx context.Context, guestIP, sandboxID string) error {
	body, _ := json.Marshal(agentapi.GuestIdentityRequest{SandboxID: sandboxID})
	url := fmt.Sprintf("http://%s:%d/identity", guestIP, agentapi.Port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, Transport: guestOneShotTransport}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("initialize guest identity on %s: %w", guestIP, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("agent on %s has no /identity (old sandboxd — re-run install-agent)", guestIP)
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("initialize guest identity on %s: HTTP %d: %s", guestIP, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// setGuestSnapshotPoll arms the thaw agent's short MMDS poll immediately
// before Pause+Snapshot. The armed state is captured in guest memory, so a
// clone observes its new identity promptly without making every running guest
// poll MMDS aggressively. Old agents return 404 and retain the safe legacy
// behavior.
func setGuestSnapshotPoll(ctx context.Context, guestIP string, armed bool) {
	body, _ := json.Marshal(agentapi.SnapshotPollRequest{Armed: armed})
	url := fmt.Sprintf("http://%s:%d/snapshot-poll", guestIP, agentapi.Port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second, Transport: guestOneShotTransport}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "snapshot poll arm=%t %s: %v\n", armed, guestIP, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "snapshot poll arm=%t %s: HTTP %d: %s\n",
			armed, guestIP, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// installSSHKey pushes an SSH public key to the guest agent's POST /ssh-key so
// the sandbox is reachable over SSH the moment create returns. Called after the
// readiness gate on both the cold and hot (golden-clone) create paths. Unlike
// syncGuestClock this is NOT best-effort: a caller that asked for a key expects
// it, so the error is returned and the caller fails the create. A baked agent
// too old to know /ssh-key answers 404 — surfaced as an error telling the
// operator to re-run install-agent.
func installSSHKey(ctx context.Context, guestIP, pubkey string) error {
	body, _ := json.Marshal(agentapi.SSHKeyRequest{PublicKey: pubkey})
	url := fmt.Sprintf("http://%s:%d/ssh-key", guestIP, agentapi.Port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// 30 s, not 5 s: this call now also does the guest's Ed25519 host-key
	// generation and sshd start, which /identity deliberately stopped doing on
	// every create (~148 ms idle, ~685 ms under a 16-way fanout, on sandboxes
	// that mostly never use SSH). Guest CPU is the fanout bottleneck, so under a
	// burst that work can be scheduled late, and 5 s sat close enough to the
	// tail to turn a slow key into a failed create.
	client := &http.Client{Timeout: 30 * time.Second, Transport: guestOneShotTransport}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("install ssh key on %s: %w", guestIP, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("agent on %s has no /ssh-key (old sandboxd — re-run install-agent)", guestIP)
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("install ssh key on %s: HTTP %d: %s", guestIP, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// waitForAgent polls the guest agent's /health until it responds or the
// deadline passes. A fresh VM needs a few seconds for systemd to bring the
// network and sandboxd up.
func waitForAgent(ctx context.Context, guestIP string, deadline time.Duration) error {
	url := fmt.Sprintf("http://%s:%d/health", guestIP, agentapi.Port)
	// A ready agent answers over the host bridge in well under a millisecond.
	// Keep each attempt short so a missing neighbor/FDB entry or a booting NIC
	// cannot pin the readiness path to Linux's one-second ARP retransmit timer.
	probe := &http.Client{Timeout: 100 * time.Millisecond, Transport: guestOneShotTransport}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := probe.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent not ready after %s: %w", deadline, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
