package edge

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseHostname(t *testing.T) {
	id, port, err := ParseHostname("3000-550e8400-e29b-41d4-a716-446655440000.sb.example.com", "sb.example.com")
	if err != nil || id != "550e8400-e29b-41d4-a716-446655440000" || port != 3000 {
		t.Fatalf("ParseHostname = %q, %d, %v", id, port, err)
	}
	for _, bad := range []string{
		"3000.sb.example.com",
		"0-id.sb.example.com",
		"3000-id.other.example.com",
		"3000-bad_id.sb.example.com",
	} {
		if _, _, err := ParseHostname(bad, "sb.example.com"); err == nil {
			t.Errorf("ParseHostname(%q) unexpectedly succeeded", bad)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveSingleFlightAndCache(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &http.Response{
			StatusCode: 200,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"host_addr":"worker:8080","token":"host-token","ttl":5}`)),
		}, nil
	})}
	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt, err := s.resolve(context.Background(), "sandbox-id", false)
			if err != nil || rt.HostAddr != "worker:8080" {
				t.Errorf("resolve = %+v, %v", rt, err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("gateway calls = %d, want single flight call", got)
	}
	if _, err := s.resolve(context.Background(), "sandbox-id", false); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached resolve made gateway call; calls=%d", got)
	}
}

func TestNegativeCache(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: 404,
			Status:     "404 Not Found",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"unknown"}`)),
		}, nil
	})}
	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.resolve(context.Background(), "missing", false); err == nil {
			t.Fatal("missing route unexpectedly resolved")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("negative cache calls=%d, want 1", got)
	}
}

func TestOpenTunnelUsesResolvedWorkerCredential(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"host_addr":"worker:8080","token":"worker-token","ttl":5}`)),
		}, nil
	})}
	edgeSide, workerSide := net.Pipe()
	workerDone := make(chan error, 1)
	go func() {
		defer workerSide.Close()
		br := bufio.NewReader(workerSide)
		req, err := http.ReadRequest(br)
		if err != nil {
			workerDone <- err
			return
		}
		if req.Method != http.MethodConnect ||
			req.URL.Path != "/sandboxes/sandbox-id/connect/3000" ||
			req.Header.Get("Authorization") != "Bearer worker-token" {
			workerDone <- &statusError{status: 500, err: io.ErrUnexpectedEOF}
			return
		}
		if _, err := io.WriteString(workerSide,
			"HTTP/1.1 200 Connection Established\r\nX-Sandbox-Wake-Seconds: 0.125\r\n\r\n"); err != nil {
			workerDone <- err
			return
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil {
			workerDone <- err
			return
		}
		_, err = workerSide.Write([]byte("pong"))
		workerDone <- err
	}()

	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
		HTTPClient: client,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return edgeSide, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, status, err := s.openTunnel(context.Background(), "sandbox-id", 3000, false)
	if err != nil || status != 200 {
		t.Fatalf("openTunnel status=%d err=%v", status, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "pong" {
		t.Fatalf("response=%q err=%v", got, err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if s.met.wakeCount.Load() != 1 ||
		s.met.wakeNanos.Load() != int64(125*time.Millisecond) {
		t.Fatalf("wake metrics count=%d nanos=%d", s.met.wakeCount.Load(), s.met.wakeNanos.Load())
	}
}

func TestRawRouteSingleFlightCache(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: 200, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"sandbox_id":"sandbox-id","guest_port":22,"host_addr":"worker:8080","token":"tok","ttl":5}`)),
		}, nil
	})}
	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rt, err := s.resolveRaw(context.Background(), 20000, false)
		if err != nil || rt.SandboxID != "sandbox-id" || rt.GuestPort != 22 {
			t.Fatalf("raw route=%+v err=%v", rt, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("raw gateway calls=%d", calls.Load())
	}
}

func TestHealthTracksReadiness(t *testing.T) {
	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleHealth(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-ready health=%d", w.Code)
	}
	s.ready.Store(true)
	w = httptest.NewRecorder()
	s.handleHealth(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("ready health=%d", w.Code)
	}
}

func TestConnectionLimits(t *testing.T) {
	s, err := New(Config{
		Domain: "sb.example.com", PlainHTTP: true,
		GatewayURL: "http://gateway", GatewayToken: "gateway-token",
		FirstHitRate: 2, MaxConnections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}
	if !s.allowSource(addr) || !s.allowSource(addr) || s.allowSource(addr) {
		t.Fatal("per-source connection rate was not enforced")
	}
	if !s.tryAcquireConn() || s.tryAcquireConn() {
		t.Fatal("global connection ceiling was not enforced")
	}
	s.releaseConn()
	if !s.tryAcquireConn() {
		t.Fatal("released connection slot was not reusable")
	}
	s.releaseConn()
}

func TestCertificateReloadKeepsLastGoodPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeTestCertificate(t, certPath, keyPath, 1)
	var met metrics
	reloader, err := newCertificateReloader(certPath, keyPath, &met)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := reloader.GetCertificate(nil)
	if err := os.WriteFile(keyPath, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := reloader.GetCertificate(nil)
	if err != nil || got.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) != 0 {
		t.Fatalf("bad partial update replaced last good certificate: cert=%v err=%v", got, err)
	}
	writeTestCertificate(t, certPath, keyPath, 2)
	got, err = reloader.GetCertificate(nil)
	if err != nil || got.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("valid update was not loaded: serial=%v err=%v", got.Leaf.SerialNumber, err)
	}
	if met.certReloadErr.Load() != 1 || met.certReloadOK.Load() != 1 {
		t.Fatalf("reload metrics: ok=%d error=%d", met.certReloadOK.Load(), met.certReloadErr.Load())
	}
}

func writeTestCertificate(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "sb.example.com"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		DNSNames: []string{"*.sb.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000+serial, 0)
	if err := os.Chtimes(certPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}
