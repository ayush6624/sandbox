package gateway

import (
	"net/http"
	"time"
)

const edgeRouteTTL = 5 * time.Second

type edgeRoute struct {
	HostAddr string `json:"host_addr"`
	Token    string `json:"token"`
	TTL      int    `json:"ttl"`
}

// handleRoute resolves a sandbox to the worker API address and bearer token
// used by the public edge. It follows the same cross-host adopt path as normal
// id-scoped requests, but does not proxy any user data through the gateway.
func (g *Gateway) handleRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("id")
	g.mu.RLock()
	hid := g.route[id]
	h := g.hosts[hid]
	var snap host
	if h != nil && time.Since(h.lastSeen) <= g.ttl {
		snap = *h
	} else {
		h = nil
	}
	g.mu.RUnlock()

	outcome := resolveAdopted
	if h == nil {
		// The public edge calls this on every cache miss for an unknown
		// hostname, so it takes the bounded, rate-limited, id-screened policy:
		// an unresolvable label must cost the control plane ~nothing.
		var adopted string
		if adopted, outcome = g.resolveViaAdopt(id, nil, edgeResolve); outcome == resolveAdopted {
			g.mu.RLock()
			if ah := g.hosts[adopted]; ah != nil && time.Since(ah.lastSeen) <= g.ttl {
				snap = *ah
				h = ah
			}
			g.mu.RUnlock()
			if h == nil {
				outcome = resolveUnknown
			}
		}
	}
	if h == nil {
		// 404 only when provably absent. The edge negative-caches a 404 but not
		// a 503, so mislabelling an in-flight adopt here would make the edge
		// remember a live sandbox as dead for its whole negative TTL.
		writeResolveFailure(w, r, id, outcome)
		return
	}
	writeJSON(w, http.StatusOK, edgeRoute{
		HostAddr: dialAddr(snap.addr),
		Token:    snap.token,
		TTL:      int(edgeRouteTTL / time.Second),
	})
}
