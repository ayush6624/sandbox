package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/ayush6624/web-sandbox/internal/agentapi"
	"github.com/ayush6624/web-sandbox/internal/registry"
)

// Client is a thin HTTP client that talks to the websandbox server over a Unix socket.
type Client struct {
	http *http.Client
}

// New returns a client that dials socketPath.
func New(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// CreateOpts customizes sandbox creation.
type CreateOpts struct {
	// TimeoutSec auto-destroys the sandbox after this many seconds; 0 = no expiry.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// Create asks the server to provision a new sandbox.
func (c *Client) Create(ctx context.Context, opts CreateOpts) (registry.Sandbox, error) {
	var body any
	if opts.TimeoutSec > 0 {
		body = opts
	}
	var sb registry.Sandbox
	if err := c.do(ctx, "POST", "/sandboxes", body, &sb); err != nil {
		return registry.Sandbox{}, err
	}
	return sb, nil
}

// List returns all running sandboxes.
func (c *Client) List(ctx context.Context) ([]registry.Sandbox, error) {
	var out []registry.Sandbox
	if err := c.do(ctx, "GET", "/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns a single sandbox by ID.
func (c *Client) Get(ctx context.Context, id string) (registry.Sandbox, error) {
	var sb registry.Sandbox
	if err := c.do(ctx, "GET", "/sandboxes/"+id, nil, &sb); err != nil {
		return registry.Sandbox{}, err
	}
	return sb, nil
}

// Destroy stops and removes a sandbox.
func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/sandboxes/"+id, nil, nil)
}

// Exec runs a shell command inside the sandbox via the guest agent.
func (c *Client) Exec(ctx context.Context, id string, req agentapi.ExecRequest) (agentapi.ExecResult, error) {
	var res agentapi.ExecResult
	if err := c.do(ctx, "POST", "/sandboxes/"+id+"/exec", req, &res); err != nil {
		return agentapi.ExecResult{}, err
	}
	return res, nil
}

// ExecStream runs a shell command via POST /exec/stream, invoking onEvent
// (if non-nil) for every NDJSON event as it arrives. The returned ExecResult
// carries the exit fields from the final event plus the full accumulated
// stdout/stderr.
func (c *Client) ExecStream(ctx context.Context, id string, req agentapi.ExecRequest, onEvent func(agentapi.ExecEvent)) (agentapi.ExecResult, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return agentapi.ExecResult{}, err
	}
	resp, err := c.doRaw(ctx, "POST", "/sandboxes/"+id+"/exec/stream", bytes.NewReader(b))
	if err != nil {
		return agentapi.ExecResult{}, err
	}
	defer resp.Body.Close()

	var stdout, stderr strings.Builder
	var res agentapi.ExecResult
	sawExit := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev agentapi.ExecEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return res, fmt.Errorf("decode stream event: %w", err)
		}
		if onEvent != nil {
			onEvent(ev)
		}
		switch ev.Type {
		case agentapi.EventStdout:
			stdout.WriteString(ev.Data)
		case agentapi.EventStderr:
			stderr.WriteString(ev.Data)
		case agentapi.EventExit:
			res.ExitCode = ev.ExitCode
			res.TimedOut = ev.TimedOut
			res.DurationMS = ev.DurationMS
			sawExit = true
		}
	}
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("read stream: %w", err)
	}
	if !sawExit {
		return res, errors.New("stream ended without exit event")
	}
	return res, nil
}

// DialShell opens an interactive PTY WebSocket to the sandbox's shell. The
// caller owns the returned connection and must Close it. cols/rows seed the
// initial window size (0 lets the agent default to 80x24).
func (c *Client) DialShell(ctx context.Context, id string, cols, rows uint16) (*websocket.Conn, error) {
	q := url.Values{}
	if cols > 0 {
		q.Set("cols", fmt.Sprint(cols))
	}
	if rows > 0 {
		q.Set("rows", fmt.Sprint(rows))
	}
	// The host is ignored — c.http's transport dials the configured socket — but
	// must be present for a valid ws:// URL.
	u := "ws://websandbox/sandboxes/" + id + "/shell"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return nil, fmt.Errorf("dial shell (is `websandbox serve` running?): %w", err)
	}
	conn.SetReadLimit(1 << 20)
	return conn, nil
}

// ExposePort forwards an extra guest port to a host port (idempotent).
func (c *Client) ExposePort(ctx context.Context, id string, guestPort int) (registry.PortMapping, error) {
	var pm registry.PortMapping
	body := map[string]int{"guest_port": guestPort}
	if err := c.do(ctx, "POST", "/sandboxes/"+id+"/ports", body, &pm); err != nil {
		return registry.PortMapping{}, err
	}
	return pm, nil
}

// ListPorts returns every forwarded port of a sandbox, including the primary one.
func (c *Client) ListPorts(ctx context.Context, id string) ([]registry.PortMapping, error) {
	var out []registry.PortMapping
	if err := c.do(ctx, "GET", "/sandboxes/"+id+"/ports", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReadFile streams a file out of the sandbox. The caller must Close the reader.
func (c *Client) ReadFile(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.doRaw(ctx, "GET", filePath(id, "files", path), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// WriteFile writes body to path inside the sandbox, creating parent dirs.
func (c *Client) WriteFile(ctx context.Context, id, path string, body io.Reader) error {
	resp, err := c.doRaw(ctx, "PUT", filePath(id, "files", path), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ListDir lists a directory inside the sandbox.
func (c *Client) ListDir(ctx context.Context, id, path string) ([]agentapi.DirEntry, error) {
	var out []agentapi.DirEntry
	if err := c.do(ctx, "GET", filePath(id, "dir", path), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func filePath(id, endpoint, path string) string {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	return "/sandboxes/" + id + "/" + endpoint + "?" + q.Encode()
}

// doRaw issues a request with a raw body and returns the response on 2xx.
func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dial server (is `websandbox serve` running?): %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("server: %s", e.Error)
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, rdr)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dial server (is `websandbox serve` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("server: %s", e.Error)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
