package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeMetricsHost serves a minimal /metrics exposition (requiring the host
// token) so the federation scrape has something realistic to fold in.
func fakeMetricsHost(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer htok" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// parseSeries turns exposition text into {series-identifier: value}, keyed by
// the full name+labels so labeled series are distinguishable.
func parseSeries(t *testing.T, body string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			t.Fatalf("malformed line: %q", line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("value in %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// TestHandleHostMetricsFederation: two live hosts (one up, one down) plus a
// stale host. The up host's series must be re-exported with a host label, the
// down host must show scrape_ok=0, and the stale host must not appear at all.
func TestHandleHostMetricsFederation(t *testing.T) {
	up := fakeMetricsHost(t, strings.Join([]string{
		"# HELP sandbox_running Sandboxes running on this host.",
		"# TYPE sandbox_running gauge",
		"sandbox_running 2",
		"# HELP sandbox_pool_used Resources held per pool.",
		"# TYPE sandbox_pool_used gauge",
		"sandbox_pool_used{pool=\"tap\"} 2",
		"# HELP sandbox_creates_ok_total Creates that succeeded.",
		"# TYPE sandbox_creates_ok_total counter",
		"sandbox_creates_ok_total 7",
	}, "\n")+"\n")

	g := New("tok", 20*time.Second, 0, 0)
	addTestHost(g, "up", strings.TrimPrefix(up.URL, "http://"), 2, 22)
	// A host whose addr won't connect (server never started).
	addTestHost(g, "down", "127.0.0.1:1", 0, 24)
	// A stale host (past TTL) must be excluded from the scrape entirely.
	stale := addTestHost(g, "stale", strings.TrimPrefix(up.URL, "http://"), 0, 24)
	stale.lastSeen = time.Now().Add(-time.Hour)

	rr := httptest.NewRecorder()
	g.handleHostMetrics(rr, httptest.NewRequest("GET", "/metrics/hosts", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rr.Body.String()

	// Prometheus requires a family's TYPE line to appear exactly once even when
	// samples come from many hosts. Assert we grouped rather than concatenated.
	if n := strings.Count(body, "# TYPE sandbox_running gauge"); n != 1 {
		t.Fatalf("sandbox_running TYPE appears %d times, want 1 (families must be grouped)", n)
	}

	m := parseSeries(t, body)
	checks := map[string]float64{
		"sandbox_running{host=\"up\"}":                2,
		"sandbox_pool_used{host=\"up\",pool=\"tap\"}": 2,
		"sandbox_creates_ok_total{host=\"up\"}":       7,
		"sandbox_host_scrape_ok{host=\"up\"}":         1,
		"sandbox_host_scrape_ok{host=\"down\"}":       0,
	}
	for k, want := range checks {
		if got, ok := m[k]; !ok {
			t.Errorf("missing series %q", k)
		} else if got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if _, ok := m["sandbox_host_scrape_ok{host=\"stale\"}"]; ok {
		t.Errorf("stale host must be excluded from federation")
	}
	if _, ok := m["sandbox_running{host=\"down\"}"]; ok {
		t.Errorf("down host has no series beyond scrape_ok")
	}
}
