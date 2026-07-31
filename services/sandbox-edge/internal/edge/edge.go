// Package edge implements the public sandbox data plane. It depends only on
// the gateway's GET /route/{id} contract and the worker's CONNECT tunnel, so
// the command can be split into its own module/service without importing the
// worker or gateway implementations.
package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	ListenAddr       string
	HTTPAddr         string
	MetricsAddr      string
	Domain           string
	CertFile         string
	KeyFile          string
	PlainHTTP        bool
	GatewayURL       string
	GatewayToken     string
	CacheTTL         time.Duration
	NegativeTTL      time.Duration
	DialTimeout      time.Duration
	DrainTimeout     time.Duration
	HandshakeTimeout time.Duration
	MaxConnections   int
	RawListenIP      string
	RawPortMin       int
	RawPortMax       int
	FirstHitRate     int
	HTTPClient       *http.Client
	DialContext      func(context.Context, string, string) (net.Conn, error)
}

type Service struct {
	cfg Config

	cacheMu   sync.Mutex
	cache     map[string]cacheEntry
	flight    map[string]*resolveFlight
	met       metrics
	ready     atomic.Bool
	connWG    sync.WaitGroup
	acceptWG  sync.WaitGroup
	connSlots chan struct{}
	certs     *certificateReloader
	rawMu     sync.Mutex
	rawCache  map[int]rawCacheEntry
	rawFlight map[int]*rawResolveFlight
	rateMu    sync.Mutex
	rate      map[string]rateWindow
}

type route struct {
	HostAddr string `json:"host_addr"`
	Token    string `json:"token"`
	TTL      int    `json:"ttl"`
}

type cacheEntry struct {
	route  route
	err    error
	expiry time.Time
}

type resolveFlight struct {
	done  chan struct{}
	route route
	err   error
}

type metrics struct {
	open           atomic.Int64
	totalOK        atomic.Int64
	totalError     atomic.Int64
	bytesIn        atomic.Int64
	bytesOut       atomic.Int64
	resolveHit     atomic.Int64
	resolveMiss    atomic.Int64
	resolveStale   atomic.Int64
	resolveUnknown atomic.Int64
	tlsOK          atomic.Int64
	tlsError       atomic.Int64
	wakeCount      atomic.Int64
	wakeNanos      atomic.Int64
	wakeBuckets    [8]atomic.Int64
	certReloadOK   atomic.Int64
	certReloadErr  atomic.Int64
	certExpiryUnix atomic.Int64
	rawOpen        atomic.Int64
	rawOK          atomic.Int64
	rawError       atomic.Int64
	rawBytesIn     atomic.Int64
	rawBytesOut    atomic.Int64
	rawRateLimited atomic.Int64
	rateLimited    atomic.Int64
	rawResolveHit  atomic.Int64
	rawResolveMiss atomic.Int64
}

func New(cfg Config) (*Service, error) {
	cfg.Domain = strings.Trim(strings.ToLower(strings.TrimSpace(cfg.Domain)), ".")
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":443"
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = "127.0.0.1:9091"
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Second
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 95 * time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 5 * time.Minute
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 100000
	}
	if cfg.RawPortMin != 0 || cfg.RawPortMax != 0 {
		if cfg.RawPortMin < 1 || cfg.RawPortMax > 65535 || cfg.RawPortMin > cfg.RawPortMax {
			return nil, fmt.Errorf("invalid raw port range %d-%d", cfg.RawPortMin, cfg.RawPortMax)
		}
	}
	if cfg.FirstHitRate <= 0 {
		cfg.FirstHitRate = 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.DialContext == nil {
		cfg.DialContext = (&net.Dialer{}).DialContext
	}
	if cfg.Domain == "" {
		return nil, errors.New("edge domain is required")
	}
	if cfg.GatewayURL == "" || cfg.GatewayToken == "" {
		return nil, errors.New("gateway URL and token are required")
	}
	if !cfg.PlainHTTP && (cfg.CertFile == "" || cfg.KeyFile == "") {
		return nil, errors.New("TLS cert and key are required unless plain HTTP mode is enabled")
	}
	gatewayURL, err := url.ParseRequestURI(cfg.GatewayURL)
	if err != nil || (gatewayURL.Scheme != "http" && gatewayURL.Scheme != "https") || gatewayURL.Host == "" {
		if err == nil {
			err = errors.New("must be an absolute http(s) URL")
		}
		return nil, fmt.Errorf("invalid gateway URL: %w", err)
	}
	return &Service{
		cfg: cfg, cache: map[string]cacheEntry{}, flight: map[string]*resolveFlight{},
		rawCache: map[int]rawCacheEntry{}, rawFlight: map[int]*rawResolveFlight{},
		rate: map[string]rateWindow{}, connSlots: make(chan struct{}, cfg.MaxConnections),
	}, nil
}

func (s *Service) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer ln.Close()

	if !s.cfg.PlainHTTP {
		s.certs, err = newCertificateReloader(s.cfg.CertFile, s.cfg.KeyFile, &s.met)
		if err != nil {
			return fmt.Errorf("load edge certificate: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			GetCertificate: s.certs.GetCertificate,
			MinVersion:     tls.VersionTLS12,
			// Routing is per TCP connection. Advertising h2 would permit
			// cross-hostname connection coalescing and misroute streams.
			NextProtos: []string{"http/1.1"},
		})
	}

	errc := make(chan error, 3)
	s.acceptWG.Add(1)
	go func() {
		defer s.acceptWG.Done()
		s.acceptLoop(ctx, ln, errc)
	}()
	rawListeners, err := s.openRawListeners(ctx, errc)
	if err != nil {
		return err
	}
	defer closeListeners(rawListeners)
	go s.serveMetrics(ctx, errc)
	if s.cfg.HTTPAddr != "" && !s.cfg.PlainHTTP {
		go s.serveRedirects(ctx, errc)
	}
	s.ready.Store(true)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	s.ready.Store(false)
	_ = ln.Close()
	closeListeners(rawListeners)
	s.acceptWG.Wait()
	drained := make(chan struct{})
	go func() {
		s.connWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(s.cfg.DrainTimeout):
	}
	return serveErr
}

func (s *Service) acceptLoop(ctx context.Context, ln net.Listener, errc chan<- error) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			errc <- err
			return
		}
		if !s.allowSource(conn.RemoteAddr()) {
			s.met.rateLimited.Add(1)
			s.met.totalError.Add(1)
			_ = conn.Close()
			continue
		}
		if !s.tryAcquireConn() {
			s.met.totalError.Add(1)
			_ = conn.Close()
			continue
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer s.releaseConn()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Service) handleConn(parent context.Context, client net.Conn) {
	defer client.Close()
	s.met.open.Add(1)
	defer s.met.open.Add(-1)

	var host string
	var prefix []byte
	if tc, ok := client.(*tls.Conn); ok {
		handshakeCtx, cancel := context.WithTimeout(parent, s.cfg.HandshakeTimeout)
		err := tc.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			s.met.tlsError.Add(1)
			s.met.totalError.Add(1)
			return
		}
		s.met.tlsOK.Add(1)
		host = tc.ConnectionState().ServerName
	} else {
		var err error
		_ = client.SetReadDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
		host, prefix, err = readHTTPHost(client)
		_ = client.SetReadDeadline(time.Time{})
		if err != nil {
			s.writeError(client, http.StatusBadRequest, err)
			s.met.totalError.Add(1)
			return
		}
	}

	id, port, err := ParseHostname(host, s.cfg.Domain)
	if err != nil {
		s.writeError(client, http.StatusNotFound, err)
		s.met.totalError.Add(1)
		return
	}

	ctx, cancel := context.WithTimeout(parent, s.cfg.DialTimeout)
	defer cancel()
	backend, status, err := s.openTunnel(ctx, id, port, false)
	var workerErr *workerStatusError
	if err != nil && (status == 0 || (errors.As(err, &workerErr) && status == http.StatusNotFound)) {
		s.invalidate(id)
		backend, status, err = s.openTunnel(ctx, id, port, true)
	}
	if err != nil {
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.writeError(client, status, err)
		s.met.totalError.Add(1)
		return
	}
	defer backend.Close()
	if len(prefix) > 0 {
		if _, err := backend.Write(prefix); err != nil {
			s.met.totalError.Add(1)
			return
		}
		s.met.bytesIn.Add(int64(len(prefix)))
	}
	s.met.totalOK.Add(1)
	s.pipe(client, backend)
}

func (s *Service) tryAcquireConn() bool {
	select {
	case s.connSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Service) releaseConn() {
	<-s.connSlots
}

func ParseHostname(host, domain string) (string, int, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + strings.Trim(strings.ToLower(domain), ".")
	if !strings.HasSuffix(host, suffix) {
		return "", 0, fmt.Errorf("hostname is outside ingress domain")
	}
	label := strings.TrimSuffix(host, suffix)
	portText, id, ok := strings.Cut(label, "-")
	if !ok || id == "" || strings.Contains(id, ".") {
		return "", 0, fmt.Errorf("invalid sandbox hostname")
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", 0, fmt.Errorf("invalid sandbox id")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid sandbox port")
	}
	return id, port, nil
}

func (s *Service) openTunnel(ctx context.Context, id string, port int, force bool) (net.Conn, int, error) {
	rt, err := s.resolve(ctx, id, force)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) {
			return nil, se.status, err
		}
		return nil, 0, err
	}
	return s.connectWorker(ctx, rt, id, port)
}

func (s *Service) connectWorker(ctx context.Context, rt route, id string, port int) (net.Conn, int, error) {
	conn, err := s.cfg.DialContext(ctx, "tcp", rt.HostAddr)
	if err != nil {
		return nil, 0, err
	}
	req := fmt.Sprintf("CONNECT /sandboxes/%s/connect/%d HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n",
		id, port, rt.HostAddr, rt.Token)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, 0, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		conn.Close()
		return nil, resp.StatusCode, &workerStatusError{
			status: resp.StatusCode,
			err:    fmt.Errorf("worker tunnel: %s: %s", resp.Status, strings.TrimSpace(string(msg))),
		}
	}
	if seconds, err := strconv.ParseFloat(resp.Header.Get("X-Sandbox-Wake-Seconds"), 64); err == nil && seconds >= 0 {
		s.met.observeWake(seconds)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, http.StatusOK, nil
	}
	return conn, http.StatusOK, nil
}

type workerStatusError struct {
	status int
	err    error
}

func (e *workerStatusError) Error() string { return e.err.Error() }
func (e *workerStatusError) Unwrap() error { return e.err }

type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func (s *Service) resolve(ctx context.Context, id string, force bool) (route, error) {
	now := time.Now()
	s.cacheMu.Lock()
	if !force {
		if ent, ok := s.cache[id]; ok {
			if now.Before(ent.expiry) {
				s.met.resolveHit.Add(1)
				s.cacheMu.Unlock()
				return ent.route, ent.err
			}
			s.met.resolveStale.Add(1)
			delete(s.cache, id)
		}
	}
	if fl := s.flight[id]; fl != nil {
		s.cacheMu.Unlock()
		select {
		case <-ctx.Done():
			return route{}, ctx.Err()
		case <-fl.done:
			return fl.route, fl.err
		}
	}
	fl := &resolveFlight{done: make(chan struct{})}
	s.flight[id] = fl
	s.met.resolveMiss.Add(1)
	s.cacheMu.Unlock()

	fl.route, fl.err = s.fetchRoute(ctx, id)
	ttl := s.cfg.CacheTTL
	if fl.route.TTL > 0 && time.Duration(fl.route.TTL)*time.Second < ttl {
		ttl = time.Duration(fl.route.TTL) * time.Second
	}
	if fl.err != nil {
		var se *statusError
		if errors.As(fl.err, &se) && se.status == http.StatusNotFound {
			ttl = s.cfg.NegativeTTL
			s.met.resolveUnknown.Add(1)
		} else {
			ttl = 0
		}
	}

	s.cacheMu.Lock()
	if ttl > 0 {
		s.cache[id] = cacheEntry{route: fl.route, err: fl.err, expiry: time.Now().Add(ttl)}
	}
	delete(s.flight, id)
	close(fl.done)
	s.cacheMu.Unlock()
	return fl.route, fl.err
}

func (s *Service) fetchRoute(ctx context.Context, id string) (route, error) {
	u := strings.TrimRight(s.cfg.GatewayURL, "/") + "/route/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return route{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.GatewayToken)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return route{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return route{}, &statusError{status: resp.StatusCode, err: fmt.Errorf("resolve sandbox: %s: %s", resp.Status, strings.TrimSpace(string(msg)))}
	}
	var rt route
	if err := json.NewDecoder(resp.Body).Decode(&rt); err != nil {
		return route{}, err
	}
	if rt.HostAddr == "" || rt.Token == "" {
		return route{}, errors.New("gateway returned an incomplete route")
	}
	if strings.ContainsAny(rt.HostAddr+rt.Token, "\r\n") {
		return route{}, errors.New("gateway returned an invalid route")
	}
	return rt, nil
}

func (s *Service) invalidate(id string) {
	s.cacheMu.Lock()
	delete(s.cache, id)
	s.cacheMu.Unlock()
}

func (s *Service) pipe(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(backend, client)
		s.met.bytesIn.Add(n)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, backend)
		s.met.bytesOut.Add(n)
		closeWrite(client)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}

func readHTTPHost(conn net.Conn) (string, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	var data []byte
	buf := make([]byte, 4096)
	for len(data) < 64<<10 {
		n, err := conn.Read(buf)
		data = append(data, buf[:n]...)
		if i := strings.Index(string(data), "\r\n\r\n"); i >= 0 {
			headers := strings.Split(string(data[:i]), "\r\n")
			for _, line := range headers[1:] {
				k, v, ok := strings.Cut(line, ":")
				if ok && strings.EqualFold(strings.TrimSpace(k), "host") {
					host := strings.TrimSpace(v)
					if h, _, err := net.SplitHostPort(host); err == nil {
						host = h
					}
					return host, data, nil
				}
			}
			return "", nil, errors.New("request has no Host header")
		}
		if err != nil {
			return "", nil, err
		}
	}
	return "", nil, errors.New("HTTP headers exceed 64 KiB")
}

func (s *Service) writeError(conn net.Conn, status int, err error) {
	title := http.StatusText(status)
	body := fmt.Sprintf("<!doctype html><meta charset=utf-8><title>%s</title><h1>%s</h1><p>%s</p>",
		title, title, htmlEscape(err.Error()))
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, title, len(body), body)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
