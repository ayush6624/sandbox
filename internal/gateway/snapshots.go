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
		req, err := http.NewRequestWithContext(r.Context(), "GET", client.EndpointURL(h.addr)+"/snapshots", nil)
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

// snapshotSlotsNeeded reports how much host capacity a snapshot op consumes:
// a restore brings up one sandbox, a fanout brings up `count`, and rename /
// delete / public-fields bring up nothing.
func snapshotSlotsNeeded(r *http.Request, body []byte) int {
	if r.Method != http.MethodPost {
		return 0
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/restore"):
		return 1
	case strings.HasSuffix(r.URL.Path, "/fanout"):
		var b struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(body, &b); err != nil || b.Count <= 0 {
			return 1
		}
		return b.Count
	}
	return 0
}

// handleSnapshotOp forwards a snapshot operation to a host and captures the
// response (rather than blind-proxying) so new sandbox routes are recorded
// immediately instead of waiting a heartbeat.
//
// Ops that BRING UP SANDBOXES — restore and fanout, which is what every
// snapshot-sourced and template-sourced create is — are placed exactly like
// POST /sandboxes: reserve, wait in the shared bounded queue when the fleet is
// full, and fail over on capacity pushback. Before this they were pinned to
// whichever host owned the snapshot and rejected outright when it was full,
// which had two consequences measured on a 89-task benchmark: creates 503'd
// while other hosts had free slots, and because they never entered the queue,
// the queue-depth scale-out signal stayed flat and the fleet never grew.
//
// Ops that consume no capacity keep the cheap path: owner if alive, else any
// live host (a delete must still reach a host to remove the bucket objects
// after the creator died).
func (g *Gateway) handleSnapshotOp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	requestBody := io.Reader(http.NoBody)
	if r.Body != nil {
		requestBody = r.Body
	}
	body, err := io.ReadAll(io.LimitReader(requestBody, 1<<20))
	if err != nil {
		httpError(w, 400, fmt.Errorf("read body: %w", err))
		return
	}

	owner := g.snapshotOwner(id)
	if needed := snapshotSlotsNeeded(r, body); needed > 0 {
		g.serveSnapshotCreate(w, r, id, body, owner, needed)
		return
	}

	target := g.hostByID(owner)
	if target == nil {
		if target = g.pickHost(); target == nil {
			httpError(w, http.StatusServiceUnavailable, errors.New("no live host to serve the snapshot operation"))
			return
		}
		fmt.Fprintf(os.Stderr, "gateway: snapshot %s has no live owner; forwarding %s to %s\n", id, r.URL.Path, target.id)
	}
	status, respBody, err := g.forwardSnapshotOp(r, target, body)
	if err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	if status < 300 {
		g.recordSnapshotOp(r, id, target, status, &respBody, false)
	}
	writeSnapshotResponse(w, status, respBody)
}

// serveSnapshotCreate places a restore/fanout with the same discipline as a
// default create: reserve capacity up front, queue when there is none, and fail
// over to another host on capacity pushback.
func (g *Gateway) serveSnapshotCreate(w http.ResponseWriter, r *http.Request, id string, body []byte, owner string, needed int) {
	// One shared deadline across attempts, like handleCreate: failing over
	// twice must not multiply the client's wait.
	deadline := time.Now().Add(g.queueWait)
	tried := map[string]bool{}
	var lastErr error

	for attempt := 0; attempt < maxCreateAttempts; attempt++ {
		h := g.reserveHostFor(tried, needed, owner)
		if h == nil {
			h = g.awaitHostWith(r.Context(), deadline, needed, func() *host {
				return g.reserveHostFor(tried, needed, owner)
			})
		}
		if h == nil {
			break
		}

		status, respBody, err := g.forwardSnapshotOp(r, h, body)
		if err != nil {
			g.releaseReservationN(h, false)
			if r.Context().Err() != nil {
				httpError(w, 499, fmt.Errorf("client disconnected during snapshot create on host %s: %w", h.id, err))
				return
			}
			g.penalize(h.id, connPenalty, false)
			lastErr = err
			tried[h.id] = true
			continue
		}

		if status < 300 {
			// The reservation becomes used capacity; recordSnapshotOp must not
			// also debit the host, or the same sandboxes are counted twice.
			g.releaseReservationN(h, true)
			g.recordSnapshotOp(r, id, h, status, &respBody, true)
			writeSnapshotResponse(w, status, respBody)
			return
		}

		g.releaseReservationN(h, false)
		if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
			// The host's advertised free was stale (its own admission is the
			// authority). Stop feeding it and try elsewhere.
			g.penalize(h.id, capacityPenalty, true)
			lastErr = fmt.Errorf("host %s: %s", h.id, strings.TrimSpace(string(respBody)))
			tried[h.id] = true
			continue
		}
		// A genuine host-side failure — relay it rather than burning boots.
		writeSnapshotResponse(w, status, respBody)
		return
	}

	w.Header().Set("Retry-After", "5")
	g.rejected.Add(1)
	if lastErr != nil {
		httpError(w, http.StatusServiceUnavailable, fmt.Errorf("no host with free capacity for snapshot %s (last error: %w)", id, lastErr))
		return
	}
	httpError(w, http.StatusServiceUnavailable, fmt.Errorf("no host with free capacity for snapshot %s", id))
}

// snapshotOwner returns the id of the host currently known to hold the
// snapshot, or "" when there is none or its heartbeat has gone stale.
func (g *Gateway) snapshotOwner(id string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	hid, ok := g.snapRoute[id]
	if !ok {
		return ""
	}
	if h := g.hosts[hid]; h != nil && time.Since(h.lastSeen) <= g.ttl {
		return hid
	}
	return ""
}

func (g *Gateway) hostByID(id string) *host {
	if id == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	h := g.hosts[id]
	if h == nil || time.Since(h.lastSeen) > g.ttl {
		return nil
	}
	snap := *h
	return &snap
}

func (g *Gateway) forwardSnapshotOp(r *http.Request, target *host, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, client.EndpointURL(target.addr)+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.token)
	resp, err := snapClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("host %s unreachable: %w", target.id, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read host %s response: %w", target.id, err)
	}
	return resp.StatusCode, respBody, nil
}

func writeSnapshotResponse(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// recordSnapshotOp updates routing state after a successful snapshot op and
// annotates returned sandboxes with the serving host's address (like create).
func (g *Gateway) recordSnapshotOp(r *http.Request, snapID string, target *host, status int, respBody *[]byte, reserved bool) {
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
			g.pinRouteLocked(sb.ID)
			if hh := g.hosts[target.id]; hh != nil && !reserved {
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
				g.pinRouteLocked(sbs[i].ID)
				sbs[i].HostAddr = hostOnly(target.addr)
			}
			if hh := g.hosts[target.id]; hh != nil && !reserved {
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
