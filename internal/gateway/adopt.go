package gateway

// Cross-host wake dispatch (roadmap B4c). The gateway holds no durable state, so
// it can't itself know a hibernated sandbox's location once its owning host is
// gone. Instead, on a route miss it asks a live host to reconstruct the sandbox
// from the shared GCS record (POST /sandboxes/{id}/adopt); the host does every
// GCS touch. Drain moves a host's sandboxes elsewhere by pairing a release on
// the source with an adopt on a target.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
)

// adoptTimeout bounds one adopt dispatch (reconstruct + wake); matches the
// create-side generosity since an adopt does comparable bring-up work.
const adoptTimeout = 5 * time.Minute

// negCacheTTL is how long a definitive "no durable record" (404 from a host's
// GCS lookup) is remembered, so a burst of requests for a dead id doesn't fan an
// adopt out to every host each time.
const negCacheTTL = 5 * time.Second

// adoptInflight single-flights concurrent adopts of the same id.
type adoptInflight struct {
	done chan struct{}
	host string
	ok   bool
}

// resolveViaAdopt returns the host id that now owns id after adopting it, or
// ok=false if no host could (no durable record, or all placement attempts
// failed). Concurrent callers for the same id share one adopt. exclude names
// hosts NOT to place on (e.g. the source of a drain).
func (g *Gateway) resolveViaAdopt(id string, exclude map[string]bool) (string, bool) {
	if v, ok := g.notFound.Load(id); ok {
		if time.Now().Before(v.(time.Time)) {
			return "", false
		}
		g.notFound.Delete(id)
	}

	g.adoptMu.Lock()
	if fl, ok := g.adopts[id]; ok {
		g.adoptMu.Unlock()
		<-fl.done
		return fl.host, fl.ok
	}
	fl := &adoptInflight{done: make(chan struct{})}
	g.adopts[id] = fl
	g.adoptMu.Unlock()

	fl.host, fl.ok = g.adoptElsewhere(id, exclude)

	g.adoptMu.Lock()
	delete(g.adopts, id)
	g.adoptMu.Unlock()
	close(fl.done)
	return fl.host, fl.ok
}

// adoptElsewhere picks a live host and dispatches an adopt, failing over on
// capacity/connection errors exactly like a create. A 404 is definitive (the
// GCS record doesn't exist — every host would agree) and is negative-cached. On
// success the route is recorded and the host's slot is consumed. Runs the adopt
// on a detached, bounded context so a caller's disconnect can't abort an adopt
// other waiters depend on.
func (g *Gateway) adoptElsewhere(id string, exclude map[string]bool) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), adoptTimeout)
	defer cancel()

	tried := map[string]bool{}
	for k := range exclude {
		tried[k] = true
	}
	for attempt := 0; attempt < maxCreateAttempts; attempt++ {
		h := g.reserveHost(tried)
		if h == nil {
			return "", false // no live host with capacity to take it
		}
		_, err := client.NewHTTP(h.addr, h.token).Adopt(ctx, id)
		if err == nil {
			g.mu.Lock()
			g.route[id] = h.id
			g.mu.Unlock()
			g.release(h.id, true)
			return h.id, true
		}
		g.release(h.id, false)

		var ae *client.APIError
		switch {
		case errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound:
			// No durable record anywhere — don't try other hosts.
			g.notFound.Store(id, time.Now().Add(negCacheTTL))
			return "", false
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusServiceUnavailable || ae.StatusCode == http.StatusTooManyRequests):
			g.penalize(h.id, capacityPenalty, true) // stale free count / contended fence; try elsewhere
		case !errors.As(err, &ae):
			g.penalize(h.id, connPenalty, false) // host unreachable
		default:
			// A genuine host-side error (e.g. 400 no-bucket, 500). Not a
			// capacity signal and not a definitive not-found — stop.
			fmt.Fprintf(os.Stderr, "gateway: adopt %s on host %s: %v\n", id, h.id, err)
			return "", false
		}
		tried[h.id] = true
	}
	return "", false
}

// handleDrain moves every sandbox currently routed to a host onto other live
// hosts: release on the source (freeze + confirm durable + drop local), then
// adopt on a target (excluding the source). Sandboxes that can't be released
// (busy/pinned) or placed are left where they are and counted as skipped.
func (g *Gateway) handleDrain(w http.ResponseWriter, r *http.Request) {
	hostID := r.PathValue("host")

	g.mu.RLock()
	src := g.hosts[hostID]
	var srcSnap host
	if src != nil {
		srcSnap = *src
	}
	var ids []string
	for sid, hid := range g.route {
		if hid == hostID {
			ids = append(ids, sid)
		}
	}
	g.mu.RUnlock()
	if src == nil {
		httpError(w, http.StatusNotFound, fmt.Errorf("host %s not registered", hostID))
		return
	}

	exclude := map[string]bool{hostID: true}
	moved, skipped := 0, 0
	for _, id := range ids {
		relCtx, cancel := context.WithTimeout(context.Background(), adoptTimeout)
		err := client.NewHTTP(srcSnap.addr, srcSnap.token).Release(relCtx, id)
		cancel()
		if err != nil {
			// Busy/pinned (not durable / not freezable) or unreachable — leave it.
			skipped++
			continue
		}
		// The source no longer owns it; drop the stale route so adopt can place
		// it (and so a heartbeat race doesn't route back to the drained host).
		g.mu.Lock()
		delete(g.route, id)
		g.mu.Unlock()
		if _, ok := g.resolveViaAdopt(id, exclude); ok {
			moved++
		} else {
			skipped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host": hostID, "total": len(ids), "moved": moved, "skipped": skipped,
	})
}
