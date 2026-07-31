package edge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

func (s *Service) serveMetrics(ctx context.Context, errc chan<- error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	srv := &http.Server{Addr: s.cfg.MetricsAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errc <- fmt.Errorf("metrics listener: %w", err)
	}
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	if !s.cfg.PlainHTTP && s.met.certExpiryUnix.Load() <= time.Now().Unix() {
		http.Error(w, "certificate expired", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Service) serveRedirects(ctx context.Context, errc chan<- error) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, _, err := ParseHostname(host, s.cfg.Domain); err != nil {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	srv := &http.Server{Addr: s.cfg.HTTPAddr, Handler: handler}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errc <- fmt.Errorf("redirect listener: %w", err)
	}
}

func (s *Service) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metric := func(name, help string, value int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
	}
	metric("sandbox_edge_conns_open", "Currently open public ingress connections.", s.met.open.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_conns_total Completed public ingress connections by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_conns_total counter")
	fmt.Fprintf(w, "sandbox_edge_conns_total{result=%s} %d\n", strconv.Quote("ok"), s.met.totalOK.Load())
	fmt.Fprintf(w, "sandbox_edge_conns_total{result=%s} %d\n", strconv.Quote("error"), s.met.totalError.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_bytes_total Bytes copied by direction.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_bytes_total counter")
	fmt.Fprintf(w, "sandbox_edge_bytes_total{dir=%s} %d\n", strconv.Quote("client_to_guest"), s.met.bytesIn.Load())
	fmt.Fprintf(w, "sandbox_edge_bytes_total{dir=%s} %d\n", strconv.Quote("guest_to_client"), s.met.bytesOut.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_resolve_total Route-cache resolutions by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_resolve_total counter")
	for result, value := range map[string]int64{
		"hit": s.met.resolveHit.Load(), "miss": s.met.resolveMiss.Load(),
		"stale": s.met.resolveStale.Load(), "unknown": s.met.resolveUnknown.Load(),
	} {
		fmt.Fprintf(w, "sandbox_edge_resolve_total{result=%s} %d\n", strconv.Quote(result), value)
	}
	fmt.Fprintln(w, "# HELP sandbox_edge_tls_handshakes_total TLS handshakes by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_tls_handshakes_total counter")
	fmt.Fprintf(w, "sandbox_edge_tls_handshakes_total{result=%s} %d\n", strconv.Quote("ok"), s.met.tlsOK.Load())
	fmt.Fprintf(w, "sandbox_edge_tls_handshakes_total{result=%s} %d\n", strconv.Quote("error"), s.met.tlsError.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_wake_seconds Worker wake and guest-dial latency before tunnel establishment.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_wake_seconds histogram")
	for i, bound := range wakeBounds {
		fmt.Fprintf(w, "sandbox_edge_wake_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(bound, 'g', -1, 64), s.met.wakeBuckets[i].Load())
	}
	fmt.Fprintf(w, "sandbox_edge_wake_seconds_bucket{le=\"+Inf\"} %d\n", s.met.wakeCount.Load())
	fmt.Fprintf(w, "sandbox_edge_wake_seconds_sum %.6f\n", float64(s.met.wakeNanos.Load())/float64(time.Second))
	fmt.Fprintf(w, "sandbox_edge_wake_seconds_count %d\n", s.met.wakeCount.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_certificate_expiry_timestamp_seconds Expiry time of the active TLS certificate.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_certificate_expiry_timestamp_seconds gauge")
	fmt.Fprintf(w, "sandbox_edge_certificate_expiry_timestamp_seconds %d\n", s.met.certExpiryUnix.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_certificate_reloads_total Certificate hot reloads by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_certificate_reloads_total counter")
	fmt.Fprintf(w, "sandbox_edge_certificate_reloads_total{result=\"ok\"} %d\n", s.met.certReloadOK.Load())
	fmt.Fprintf(w, "sandbox_edge_certificate_reloads_total{result=\"error\"} %d\n", s.met.certReloadErr.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_raw_conns_open Currently open raw TCP ingress connections.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_raw_conns_open gauge")
	fmt.Fprintf(w, "sandbox_edge_raw_conns_open %d\n", s.met.rawOpen.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_raw_conns_total Completed raw TCP connections by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_raw_conns_total counter")
	fmt.Fprintf(w, "sandbox_edge_raw_conns_total{result=\"ok\"} %d\n", s.met.rawOK.Load())
	fmt.Fprintf(w, "sandbox_edge_raw_conns_total{result=\"error\"} %d\n", s.met.rawError.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_raw_bytes_total Raw TCP bytes copied by direction.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_raw_bytes_total counter")
	fmt.Fprintf(w, "sandbox_edge_raw_bytes_total{dir=\"client_to_guest\"} %d\n", s.met.rawBytesIn.Load())
	fmt.Fprintf(w, "sandbox_edge_raw_bytes_total{dir=\"guest_to_client\"} %d\n", s.met.rawBytesOut.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_raw_resolve_total Raw route resolutions by result.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_raw_resolve_total counter")
	fmt.Fprintf(w, "sandbox_edge_raw_resolve_total{result=\"hit\"} %d\n", s.met.rawResolveHit.Load())
	fmt.Fprintf(w, "sandbox_edge_raw_resolve_total{result=\"miss\"} %d\n", s.met.rawResolveMiss.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_raw_rate_limited_total Raw first hits rejected by source rate limit.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_raw_rate_limited_total counter")
	fmt.Fprintf(w, "sandbox_edge_raw_rate_limited_total %d\n", s.met.rawRateLimited.Load())
	fmt.Fprintln(w, "# HELP sandbox_edge_rate_limited_total HTTPS first hits rejected by source rate limit.")
	fmt.Fprintln(w, "# TYPE sandbox_edge_rate_limited_total counter")
	fmt.Fprintf(w, "sandbox_edge_rate_limited_total %d\n", s.met.rateLimited.Load())
}
