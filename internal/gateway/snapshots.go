package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/registry"
)

// Snapshot routing. Heartbeats carry each host's user snapshot IDs, giving the
// gateway a snapshot→host map beside the sandbox one. Restore/fanout/delete
// prefer the owning host (artifacts are local there). When the owning host is
// dead or unknown — the exact situation GCS durability exists for — the
// operation falls back to placement: any live host with capacity serves it and
// pulls the snapshot from the bucket itself.

// snapClient forwards snapshot ops host-side. No overall timeout: a fallback
// restore may pull gigabytes from GCS before it answers.
var snapClient = &http.Client{Transport: client.SharedTransport()}

func (g *Gateway) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	var live []host
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) <= g.ttl {
			live = append(live, *h)
		}
	}
	g.mu.RUnlock()

	out := []registry.Snapshot{}
	seen := map[string]bool{}
	for _, h := range live {
		req, err := http.NewRequestWithContext(r.Context(), "GET", "http://"+h.addr+"/snapshots", nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := snapClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gateway: list snapshots from host %s: %v\n", h.id, err)
			continue
		}
		var snaps []registry.Snapshot
		err = json.NewDecoder(resp.Body).Decode(&snaps)
		resp.Body.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "gateway: decode snapshots from host %s: %v\n", h.id, err)
			continue
		}
		// A pulled snapshot exists on several hosts — dedupe by id.
		for _, sn := range snaps {
			if !seen[sn.ID] {
				seen[sn.ID] = true
				out = append(out, sn)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

// handleSnapshotOp forwards restore/fanout/delete to the owning host when
// it's alive, else to a placed fallback host. The response is captured (not
// blind-proxied) so new sandbox routes are recorded immediately instead of
// waiting a heartbeat.
func (g *Gateway) handleSnapshotOp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	g.mu.RLock()
	var target *host
	if hid, ok := g.snapRoute[id]; ok {
		if h := g.hosts[hid]; h != nil && time.Since(h.lastSeen) <= g.ttl {
			snap := *h
			target = &snap
		}
	}
	g.mu.RUnlock()

	fallback := target == nil
	if fallback {
		// Owning host gone (or snapshot never seen): place like a create — the
		// chosen host pulls the snapshot from GCS. Deletes also land here so
		// the GCS objects get removed even after the creator died.
		if target = g.pickHost(); target == nil {
			httpError(w, http.StatusServiceUnavailable, errors.New("no live host to serve the snapshot operation"))
			return
		}
		fmt.Fprintf(os.Stderr, "gateway: snapshot %s has no live owner; forwarding %s to %s\n", id, r.URL.Path, target.id)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(w, 400, fmt.Errorf("read body: %w", err))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+target.addr+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		httpError(w, 500, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.token)
	resp, err := snapClient.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Errorf("host %s unreachable: %w", target.id, err))
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Errorf("read host %s response: %w", target.id, err))
		return
	}

	// Bookkeeping on success, before relaying.
	if resp.StatusCode < 300 {
		g.recordSnapshotOp(r, id, target, resp.StatusCode, &respBody)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// recordSnapshotOp updates routing state after a successful snapshot op and
// annotates returned sandboxes with the serving host's address (like create).
func (g *Gateway) recordSnapshotOp(r *http.Request, snapID string, target *host, status int, respBody *[]byte) {
	isRestore := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restore")
	isFanout := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fanout")

	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case r.Method == http.MethodDelete:
		delete(g.snapRoute, snapID)
		return
	case isRestore && status == http.StatusCreated:
		var sb registry.Sandbox
		if err := json.Unmarshal(*respBody, &sb); err == nil && sb.ID != "" {
			g.route[sb.ID] = target.id
			if hh := g.hosts[target.id]; hh != nil {
				hh.slotsUsed++
				if hh.slotsFree > 0 {
					hh.slotsFree--
				}
			}
			sb.HostAddr = hostOnly(target.addr)
			if b, err := json.Marshal(sb); err == nil {
				*respBody = b
			}
		}
	case isFanout && status == http.StatusCreated:
		var sbs []registry.Sandbox
		if err := json.Unmarshal(*respBody, &sbs); err == nil {
			for i := range sbs {
				g.route[sbs[i].ID] = target.id
				sbs[i].HostAddr = hostOnly(target.addr)
			}
			if hh := g.hosts[target.id]; hh != nil {
				hh.slotsUsed += len(sbs)
				hh.slotsFree -= len(sbs)
				if hh.slotsFree < 0 {
					hh.slotsFree = 0
				}
			}
			if b, err := json.Marshal(sbs); err == nil {
				*respBody = b
			}
		}
	}
	// The serving host now holds the snapshot locally (it pulled it if it had
	// to); route future ops straight there until heartbeats confirm.
	g.snapRoute[snapID] = target.id
}
