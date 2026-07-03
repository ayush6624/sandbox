package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
)

// heartbeatInterval is how often the host re-registers with the gateway. The
// gateway's stale-host TTL should be a small multiple of this.
const heartbeatInterval = 5 * time.Second

// heartbeat periodically POSTs this host's state to the gateway so it can route
// requests here. It runs for the server's lifetime. Failures are logged and
// retried on the next tick — a flaky control link must never take down a host.
func (s *Server) heartbeat(ctx context.Context) {
	advertise := s.cfg.AdvertiseAddr
	if advertise == "" {
		advertise = s.cfg.ListenAddr
	}
	hostID := s.cfg.HostID
	if hostID == "" {
		hostID, _ = os.Hostname()
	}
	if hostID == "" {
		hostID = advertise // last-resort identity
	}
	url := s.cfg.GatewayURL + "/register"
	client := &http.Client{Timeout: 5 * time.Second}

	// Send one immediately so the gateway learns about us without waiting a tick.
	s.sendHeartbeat(ctx, client, url, hostID, advertise)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sendHeartbeat(ctx, client, url, hostID, advertise)
		}
	}
}

func (s *Server) sendHeartbeat(ctx context.Context, client *http.Client, url, hostID, advertise string) {
	running, err := s.reg.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: list sandboxes: %v\n", err)
		return
	}
	ids := make([]string, len(running))
	for i, sb := range running {
		ids[i] = sb.ID
	}
	var snapIDs []string
	if snaps, err := s.reg.ListSnapshots(ctx); err == nil {
		for _, sn := range snaps {
			if !sn.Golden {
				snapIDs = append(snapIDs, sn.ID)
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "heartbeat: list snapshots: %v\n", err)
	}
	hb := cluster.Heartbeat{
		HostID:      hostID,
		Addr:        advertise,
		Token:       s.cfg.APIToken,
		SlotsTotal:  s.reg.Pools().Slots(),
		SlotsUsed:   len(running),
		SandboxIDs:  ids,
		SnapshotIDs: snapIDs,
	}
	b, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: build request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.GatewayToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.GatewayToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: post to gateway: %v\n", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "heartbeat: gateway returned %s\n", resp.Status)
	}
}
