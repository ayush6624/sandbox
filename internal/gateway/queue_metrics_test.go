package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestQueueWaitMetrics(t *testing.T) {
	for _, outcome := range []string{"reserved", "timeout", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := New("test", time.Minute, time.Second, 4)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				readyAt := time.Now().Add(500 * time.Millisecond)
				if outcome == "canceled" {
					go func() { time.Sleep(500 * time.Millisecond); cancel() }()
				}
				got := g.awaitHostWith(ctx, time.Now().Add(time.Second), 1, func() *host {
					if outcome == "reserved" && !time.Now().Before(readyAt) {
						return &host{id: "ready"}
					}
					return nil
				})
				if (got != nil) != (outcome == "reserved") {
					t.Fatalf("unexpected reservation: %v", got)
				}
				rr := httptest.NewRecorder()
				g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
				body := rr.Body.String()
				for _, label := range []string{"reserved", "timeout", "canceled"} {
					count, sum, halfSecond := "0", "0", "0"
					if label == outcome {
						count, sum, halfSecond = "1", "0.5", "1"
						if outcome == "timeout" {
							sum, halfSecond = "1", "0"
						}
					}
					for _, line := range []string{
						`sandbox_create_queue_wait_seconds_count{outcome="` + label + `"} ` + count,
						`sandbox_create_queue_wait_seconds_sum{outcome="` + label + `"} ` + sum,
						`sandbox_create_queue_wait_seconds_bucket{outcome="` + label + `",le="0.5"} ` + halfSecond,
						`sandbox_create_queue_wait_seconds_bucket{outcome="` + label + `",le="+Inf"} ` + count,
					} {
						if !strings.Contains(body, line+"\n") {
							t.Errorf("missing metric %s", line)
						}
					}
				}
				if g.queued.Load() != 0 || g.queuedUnits.Load() != 0 {
					t.Fatal("queue accounting leaked")
				}
			})
		})
	}
}

func TestQueueWaitMetricsExcludeUnadmittedRequests(t *testing.T) {
	g := New("test", time.Minute, 0, 1)
	g.awaitHostWith(context.Background(), time.Now().Add(time.Second), 1, func() *host { return nil })
	g.queueWait = time.Second
	g.queued.Store(1)
	g.awaitHostWith(context.Background(), time.Now().Add(time.Second), 1, func() *host { return nil })
	rr := httptest.NewRecorder()
	g.handleMetrics(rr, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if strings.HasPrefix(line, "sandbox_create_queue_wait_seconds_count{") && !strings.HasSuffix(line, " 0") {
			t.Fatalf("unadmitted request counted as queue wait: %s", line)
		}
	}
}
