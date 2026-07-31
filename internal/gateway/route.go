package gateway

import (
	"fmt"
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

	if h == nil {
		if adopted, ok := g.resolveViaAdopt(id, nil); ok {
			g.mu.RLock()
			if ah := g.hosts[adopted]; ah != nil && time.Since(ah.lastSeen) <= g.ttl {
				snap = *ah
				h = ah
			}
			g.mu.RUnlock()
		}
	}
	if h == nil {
		httpError(w, http.StatusNotFound, fmt.Errorf("sandbox %s not found on any host", id))
		return
	}
	writeJSON(w, http.StatusOK, edgeRoute{
		HostAddr: dialAddr(snap.addr),
		Token:    snap.token,
		TTL:      int(edgeRouteTTL / time.Second),
	})
}
