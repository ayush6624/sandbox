package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

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

// Create asks the server to provision a new sandbox.
func (c *Client) Create(ctx context.Context) (registry.Sandbox, error) {
	var sb registry.Sandbox
	if err := c.do(ctx, "POST", "/sandboxes", nil, &sb); err != nil {
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
