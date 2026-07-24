package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// agentClient talks to in-guest sandboxd agents. No overall timeout — exec
// requests are bounded by their own timeout_sec and the request context.
var agentClient = &http.Client{}

// handleAgentProxy forwards a request to the sandbox's in-guest agent,
// rewriting /sandboxes/{id}/<endpoint> to http://guestIP:agentPort/<endpoint>.
// A hibernated sandbox is woken first, so callers never see the freeze; the
// begin/done pair marks the sandbox busy (and its idle clock reset) for the
// whole request, including long-running exec streams.
func (s *Server) handleAgentProxy(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sb, err := s.ensureRunning(r.Context(), id)
		if err != nil {
			// A capacity-rejected wake (memory budget/pool) surfaces as 503 +
			// Retry-After: the sandbox stays hibernated and wakeable later.
			capacityOrHTTPError(w, statusFor(err), err)
			return
		}
		// Track only ids that exist, or bogus-id requests would leak tracker
		// entries. The unpinned gap between ensureRunning and begin is a few
		// µs — the same freeze-vs-request race the reaper already tolerates.
		done := s.act.begin(id)
		defer done()

		url := fmt.Sprintf("http://%s:%d/%s", sb.GuestIP, agentapi.Port, endpoint)
		if r.URL.RawQuery != "" {
			url += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
		if err != nil {
			httpError(w, 500, err)
			return
		}
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
		sb, err := s.ensureRunning(r.Context(), id)
		if err != nil {
			shellError(w, r, statusFor(err), err)
			return
		}
		// An open shell pins the sandbox running for its whole lifetime —
		// ServeHTTP returns when the WebSocket closes.
		done := s.act.begin(id)
		defer done()
		target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", sb.GuestIP, agentapi.Port)}
		proxy := httputil.NewSingleHostReverseProxy(target)
		// NewSingleHostReverseProxy joins paths; rewrite to the agent's /shell
		// (the incoming path is /sandboxes/{id}/shell) while preserving the
		// cols/rows/cwd query string. access_token is auth plumbing for
		// browser WebSockets (see bearerAuth) — don't leak it into the guest.
		base := proxy.Director
		proxy.Director = func(req *http.Request) {
			base(req)
			req.URL.Path = "/shell"
			if q := req.URL.Query(); q.Has("access_token") {
				q.Del("access_token")
				req.URL.RawQuery = q.Encode()
			}
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
	client := &http.Client{Timeout: 2 * time.Second}
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
	client := &http.Client{Timeout: 5 * time.Second}
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
	probe := &http.Client{Timeout: 1 * time.Second}
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
		case <-time.After(200 * time.Millisecond):
		}
	}
}
