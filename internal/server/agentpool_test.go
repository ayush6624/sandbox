package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener counts accepted TCP connections, which is how these tests
// tell "the pool reused a connection" from "it dialled a new one".
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

func newAgentStub(t *testing.T) (*countingListener, string) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := &countingListener{Listener: raw}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.Host)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln, raw.Addr().String()
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body) // drain so the conn can return to the pool
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// A recycled guest IP must never let a new sandbox inherit the previous
// owner's pooled keep-alive connection. That reuse is what surfaced as
// "502 agent unreachable: read: connection reset by peer" on exec/file writes
// under churn: the dead VM RSTs the stale connection and net/http does not
// retry a POST/PUT carrying a body.
func TestAgentPoolIsNotSharedAcrossSandboxesOnARecycledIP(t *testing.T) {
	ln, addr := newAgentStub(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext:         dialAgentAuthority,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     time.Minute,
	}}

	// Same sandbox, same address: the second call must reuse the connection —
	// this is the pooling win the transport exists for.
	first := fmt.Sprintf("http://sandbox-aaa_%s:%s/health", host, port)
	get(t, client, first)
	get(t, client, first)
	if n := ln.accepts.Load(); n != 1 {
		t.Fatalf("same sandbox should reuse one connection, got %d accepts", n)
	}

	// Different sandbox now holding the SAME address: must dial fresh.
	second := fmt.Sprintf("http://sandbox-bbb_%s:%s/health", host, port)
	get(t, client, second)
	if n := ln.accepts.Load(); n != 2 {
		t.Fatalf("recycled IP under a new sandbox must not reuse the pooled connection: got %d accepts, want 2", n)
	}

	// And a clone-path wake, which keeps the id but moves to a new IP, is a
	// distinct key too (same id, different address).
	if a, b := agentAuthority("id", "172.16.0.5"), agentAuthority("id", "172.16.0.6"); a == b {
		t.Fatalf("same id at different IPs must not share a pool key: %s", a)
	}
}

// The synthetic authority is a pool key, not something the guest should see.
func TestAgentAuthorityIsRewrittenToAPlainHostHeader(t *testing.T) {
	ln, addr := newAgentStub(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dialAgentAuthority}}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://sandbox-ccc_%s:%s/health", host, port), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = net.JoinHostPort(host, port)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != net.JoinHostPort(host, port) {
		t.Fatalf("guest saw Host %q, want the plain ip:port %q", got, net.JoinHostPort(host, port))
	}
	if ln.accepts.Load() != 1 {
		t.Fatalf("expected exactly one dial")
	}
}

func TestAgentAuthorityHelpers(t *testing.T) {
	// A plain ip:port target (no separator) still dials unchanged.
	ln, addr := newAgentStub(t)
	client := &http.Client{Transport: &http.Transport{DialContext: dialAgentAuthority}}
	get(t, client, "http://"+addr+"/health")
	if ln.accepts.Load() != 1 {
		t.Fatalf("plain ip:port target must still dial")
	}

	if got, want := agentHostPort("172.16.0.9"), "172.16.0.9:8090"; got != want {
		t.Fatalf("agentHostPort = %q, want %q", got, want)
	}
	if got, want := agentAuthority("abc", "172.16.0.9"), "abc_172.16.0.9:8090"; got != want {
		t.Fatalf("agentAuthority = %q, want %q", got, want)
	}
}
