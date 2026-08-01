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
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// adoptTimeout bounds one adopt dispatch (reconstruct + wake); matches the
// create-side generosity since an adopt does comparable bring-up work. It is
// the budget of the BACKGROUND adopt, never of an inbound request — see
// resolvePolicy.
const adoptTimeout = 5 * time.Minute

const (
	// negCacheTTL is how long a definitive "no durable record" (404 from a
	// host's GCS lookup) is remembered, so a burst of requests for a dead id
	// doesn't fan an adopt out to every host each time. It used to be 5 s,
	// which is shorter than a client's poll interval right after a delete —
	// every poll paid a fresh cross-host fan-out. The cache is bounded (see
	// negCacheMax), so a longer memory costs nothing.
	negCacheTTL = 90 * time.Second
	// negCacheMax caps the negative cache. Distinct ids are attacker-supplied
	// (the public edge resolves any hostname label as an id), so the cache must
	// have a ceiling by construction, not by hoping ids repeat.
	negCacheMax = 4096

	// requestAdoptWait bounds how long an INBOUND request waits on an adopt.
	// The adopt keeps running in the background with the full adoptTimeout
	// budget; the request just stops holding the connection open for it and
	// 404s, and the caller's retry joins the same in-flight adopt (the edge
	// re-resolves after a 1 s negative TTL). Before this, a single unknown id
	// could hold a request — including an unauthenticated public-edge
	// resolution — for up to five minutes.
	requestAdoptWait = 3 * time.Second

	// FLEET-WIDE token buckets over adopt dispatch. Single-flighting only
	// collapses concurrent misses for the SAME id; a scan of distinct hostnames
	// dispatches one cross-host fan-out each, every one of which makes a worker
	// do GCS lookups. These buckets are the brake that isn't per-source (the
	// edge's FirstHitRate is per-source and so does not bound the fleet).
	//
	// There are two, because they protect against different things and must not
	// share a budget: public ingress resolution (GET /route, /raw-route — the
	// only unauthenticated-in-origin traffic) gets its own, so a hostname scan
	// exhausts THAT bucket and cannot deny an authenticated client the
	// cross-host wake it's paying for.
	adoptEdgeBurst  = 8
	adoptEdgeRefill = 2.0
	adoptAPIBurst   = 16
	adoptAPIRefill  = 4.0
	// maxAdoptInflight caps concurrent background adopts, since each one
	// outlives the request that triggered it.
	maxAdoptInflight = 16
)

// resolvePolicy is how much a caller may spend turning an id with no live route
// into a live host. The split exists because the two callers are nothing alike:
// an inbound HTTP request (possibly unauthenticated, arriving through the public
// edge) needs a fast negative, while a drain is an operator-initiated move of
// real sandboxes that must actually complete.
type resolvePolicy struct {
	// wait bounds the caller's wait for the adopt result; 0 waits for
	// completion (bounded only by adoptTimeout).
	wait time.Duration
	// screenID rejects ids that cannot be a sandbox id before any host is
	// contacted — the cheapest possible negative.
	screenID bool
	// limiter names the fleet-wide bucket dispatch draws from (adoptNoLimit
	// draws from none).
	limiter adoptLimitClass
}

type adoptLimitClass int

const (
	adoptNoLimit adoptLimitClass = iota
	adoptLimitEdge
	adoptLimitAPI
)

var (
	// requestResolve serves authenticated inbound HTTP: id-scoped proxying and
	// the id-scoped port handlers.
	requestResolve = resolvePolicy{wait: requestAdoptWait, screenID: true, limiter: adoptLimitAPI}
	// edgeResolve serves the public ingress resolution endpoints (GET /route/{id},
	// GET /raw-route/{port}), whose ids come from whatever hostname a stranger
	// pointed at the edge.
	edgeResolve = resolvePolicy{wait: requestAdoptWait, screenID: true, limiter: adoptLimitEdge}
	// drainResolve serves POST /hosts/{host}/drain: authenticated, deliberate,
	// and moving ids the gateway itself is routing — so it keeps the long
	// budget, is not rate-limited, and skips the id screen (its ids are real by
	// construction).
	drainResolve = resolvePolicy{}
)

// resolveOutcome is a THREE-state answer, and the three states must stay
// distinguishable all the way out to the HTTP status. Collapsing "we don't know
// yet" into "does not exist" is the whole hazard: the bounded request wait
// (requestAdoptWait) makes "don't know" a routine outcome for a real,
// cross-host-hibernated sandbox, and a 404 tells the SDK to raise NotFoundError
// — a client then concludes the sandbox is GONE and stops retrying, when in fact
// its adopt was still running and would have succeeded a second later.
type resolveOutcome int

const (
	// resolveAdopted: a live host now owns the sandbox; the returned host id is
	// valid and the route is recorded.
	resolveAdopted resolveOutcome = iota
	// resolveAbsent: definitive. Either the id cannot be a sandbox id, or a host
	// read the durable store and reported no record — and every host reads the
	// same store, so no retry can change this answer. The ONLY outcome that may
	// become a 404.
	resolveAbsent
	// resolveUnknown: indeterminate — the adopt is still in flight past our
	// wait, dispatch was rate-limited, no host had capacity to take it, or a
	// host errored. The sandbox may well exist; the caller must answer with the
	// capacity-class 503 + Retry-After (close code 4503 on a WebSocket) so the
	// client retries instead of giving up.
	resolveUnknown
)

// adoptInflight single-flights concurrent adopts of the same id.
type adoptInflight struct {
	done    chan struct{}
	host    string
	outcome resolveOutcome
}

// writeResolveFailure is the ONLY place an unresolved sandbox becomes an HTTP
// status, so the three outcomes cannot be collapsed by a careless call site.
// resolveAbsent is the sole 404; everything else is the capacity-class
// 503 + Retry-After, which the SDK and the gateway's own failover already treat
// as retryable. A WebSocket upgrade gets the equivalent close code (4404/4503)
// because a browser sees nothing else.
func writeResolveFailure(w http.ResponseWriter, r *http.Request, id string, outcome resolveOutcome) {
	status := http.StatusServiceUnavailable
	err := fmt.Errorf("sandbox %s is not resolvable yet (adopt in flight or deferred); retry", id)
	if outcome == resolveAbsent {
		status = http.StatusNotFound
		err = fmt.Errorf("sandbox %s not found on any host", id)
	} else {
		w.Header().Set("Retry-After", "2")
	}
	if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseCodeFor(status), err.Error()) == nil {
		return
	}
	httpError(w, status, err)
}

// resolveViaAdopt returns the host id that now owns id after adopting it, plus
// the outcome the caller MUST branch on (see resolveOutcome — an absent/unknown
// mix-up is user-visible). Concurrent callers for the same id share one adopt.
// exclude names hosts NOT to place on (e.g. the source of a drain).
//
// The order matters: every cheap negative is answered before anything is
// dispatched, because this path is reachable from unauthenticated public traffic
// (audit L1).
func (g *Gateway) resolveViaAdopt(id string, exclude map[string]bool, pol resolvePolicy) (string, resolveOutcome) {
	if g.notFound.has(id) {
		// The negative cache holds ONLY definitive verdicts (a host's 404 in
		// adoptElsewhere) — never a timeout or a throttle — which is what makes
		// a cache hit safe to answer 404 with.
		g.adoptSuppressedCached.Add(1)
		return "", resolveAbsent
	}
	if pol.screenID && !wellFormedSandboxID(id) {
		// Not a shape any sandbox id has ever had, so no host can hold a
		// durable record for it: 404 without contacting anything, and without
		// even spending a negative-cache slot. This is what makes a hostname
		// scan free for the control plane.
		g.adoptSuppressedMalformed.Add(1)
		return "", resolveAbsent
	}

	g.adoptMu.Lock()
	fl, inflight := g.adopts[id]
	if !inflight {
		if !g.allowAdoptDispatch(pol.limiter, len(g.adopts)) {
			g.adoptMu.Unlock()
			g.adoptSuppressedThrottled.Add(1)
			return "", resolveUnknown // we never looked; say so
		}
		fl = &adoptInflight{done: make(chan struct{})}
		g.adopts[id] = fl
		g.adoptDispatched.Add(1)
		// Detached from the caller: a bounded-wait caller walks away while the
		// adopt finishes, and its retry (or another waiter) joins this same
		// flight instead of starting a second one.
		go g.runAdopt(id, exclude, fl)
	}
	g.adoptMu.Unlock()

	if pol.wait <= 0 {
		<-fl.done
		return fl.host, fl.outcome
	}
	t := time.NewTimer(pol.wait)
	defer t.Stop()
	select {
	case <-fl.done:
		return fl.host, fl.outcome
	case <-t.C:
		g.adoptWaitTimeouts.Add(1)
		return "", resolveUnknown // still running; a retry joins this flight
	}
}

// runAdopt performs one adopt to completion and publishes the result to every
// waiter. It runs outside any request, so the http server's recover() no longer
// covers it — a panic here would take the gateway down, hence the local
// recover: a broken adopt degrades to a failed resolution.
func (g *Gateway) runAdopt(id string, exclude map[string]bool, fl *adoptInflight) {
	defer func() {
		if v := recover(); v != nil {
			// Our bug, not evidence the sandbox is gone: unknown, so waiters
			// retry rather than being told it doesn't exist.
			fmt.Fprintf(os.Stderr, "gateway: panic in adopt %s: %v\n", id, v)
			fl.host, fl.outcome = "", resolveUnknown
		}
		g.adoptMu.Lock()
		delete(g.adopts, id)
		g.adoptMu.Unlock()
		close(fl.done)
	}()
	fl.host, fl.outcome = g.adoptElsewhere(id, exclude)
}

// allowAdoptDispatch admits one adopt dispatch. Called with adoptMu held, so
// inflight is exact. An unlimited class (drain) is admitted unconditionally —
// it is operator-initiated, serialized by its own loop, and must not start
// reporting sandboxes as skipped just because request-path adopts are busy.
func (g *Gateway) allowAdoptDispatch(class adoptLimitClass, inflight int) bool {
	switch class {
	case adoptLimitEdge:
		return inflight < maxAdoptInflight && g.edgeAdopts.allow()
	case adoptLimitAPI:
		return inflight < maxAdoptInflight && g.apiAdopts.allow()
	default:
		return true
	}
}

// tokenBucket is a lazily-refilled token bucket. Small enough not to justify a
// dependency, and it must be cheap: allow() runs on a request path whose whole
// point is answering unknown ids for ~nothing.
type tokenBucket struct {
	burst  float64
	rate   float64 // tokens per second
	mu     sync.Mutex
	tokens float64
	filled time.Time
}

func newTokenBucket(burst, rate float64) *tokenBucket {
	return &tokenBucket{burst: burst, rate: rate, tokens: burst, filled: time.Now()}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens += now.Sub(b.filled).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.filled = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// wellFormedSandboxID reports whether id has the shape of a sandbox id:
// canonical RFC 4122 8-4-4-4-12 hex, which is what registry ids are
// (uuid.NewString in internal/server). Keep this in step with id minting — if
// ids ever gain a prefix or another format, a real sandbox would 404 on the
// adopt path (only there: a routed sandbox never reaches this).
func wellFormedSandboxID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// adoptElsewhere picks a live host and dispatches an adopt, failing over on
// capacity/connection errors exactly like a create. A 404 is definitive (the
// GCS record doesn't exist — every host would agree) and is negative-cached. On
// success the route is recorded and the host's slot is consumed. Runs the adopt
// on a detached, bounded context so a caller's disconnect can't abort an adopt
// other waiters depend on.
func (g *Gateway) adoptElsewhere(id string, exclude map[string]bool) (string, resolveOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), adoptTimeout)
	defer cancel()

	tried := map[string]bool{}
	for k := range exclude {
		tried[k] = true
	}
	for attempt := 0; attempt < maxCreateAttempts; attempt++ {
		h := g.reserveHostOrdinary(tried)
		if h == nil {
			// Nothing was asked, so nothing was learned: the sandbox may well
			// have a durable record that a host could take once capacity frees.
			return "", resolveUnknown
		}
		_, err := client.NewHTTP(h.addr, h.token).Adopt(ctx, id)
		if err == nil {
			hostID := g.landReservation(h, id)
			return hostID, resolveAdopted
		}
		g.releaseReservation(h, false)

		var ae *client.APIError
		switch {
		case errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound:
			// The host read the shared durable store and found no record. Every
			// host reads the same store, so this is the one verdict no retry can
			// change — the only path that may cache a negative and 404.
			g.notFound.add(id)
			return "", resolveAbsent
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusServiceUnavailable || ae.StatusCode == http.StatusTooManyRequests):
			g.penalize(h.id, capacityPenalty, true) // stale free count / contended fence; try elsewhere
		case !errors.As(err, &ae):
			g.penalize(h.id, connPenalty, false) // host unreachable
		default:
			// A genuine host-side error (e.g. 400 no-bucket, 500). Not a
			// capacity signal and NOT a not-found: we still don't know whether
			// the sandbox exists, so the caller must not answer 404.
			fmt.Fprintf(os.Stderr, "gateway: adopt %s on host %s: %v\n", id, h.id, err)
			return "", resolveUnknown
		}
		tried[h.id] = true
	}
	// Every attempt was a capacity or transport failure — still indeterminate.
	return "", resolveUnknown
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
		g.unpinRouteLocked(id)
		g.mu.Unlock()
		if _, outcome := g.resolveViaAdopt(id, exclude, drainResolve); outcome == resolveAdopted {
			moved++
		} else {
			skipped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"host": hostID, "total": len(ids), "moved": moved, "skipped": skipped,
	})
}
