package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/registry"
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
		if s.cfg.ManagementTransport == "tls" {
			advertise = "https://" + advertise
		} else {
			advertise = "http://" + advertise
		}
	}
	hostID := s.cfg.HostID
	if hostID == "" {
		hostID, _ = os.Hostname()
	}
	if hostID == "" {
		hostID = advertise // last-resort identity
	}
	url := strings.TrimRight(s.cfg.GatewayURL, "/") + "/internal/v1/hosts:register"
	client := &http.Client{Timeout: 5 * time.Second}

	// Golden adoption/build completes asynchronously after the heartbeat loop
	// starts. That transition changes slots_free from 0 to real capacity, so
	// send immediately instead of waiting up to heartbeatInterval. Once warmed
	// is closed it stays closed; nil the local channel after observing it to
	// avoid a permanently-ready select case and a heartbeat busy loop.
	warmed := (<-chan struct{})(s.warmed)
	poolSettled := (<-chan struct{})(s.readyPoolSettled)
	select {
	case <-warmed:
		warmed = nil
	default:
	}
	select {
	case <-poolSettled:
		poolSettled = nil
	default:
	}

	// Send one immediately so the gateway learns about us without waiting a
	// tick. Check warmed first: if it was already closed this heartbeat carries
	// capacity and no event heartbeat is needed. If it closes during this send,
	// the still-live channel case below sends the capacity update afterward.
	s.sendHeartbeat(ctx, client, url, hostID, advertise)

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-warmed:
			warmed = nil
			s.sendHeartbeat(ctx, client, url, hostID, advertise)
		case <-poolSettled:
			poolSettled = nil
			s.sendHeartbeat(ctx, client, url, hostID, advertise)
		case <-ticker.C:
			s.sendHeartbeat(ctx, client, url, hostID, advertise)
		}
	}
}

func (s *Server) sendHeartbeat(ctx context.Context, client *http.Client, url, hostID, advertise string) {
	// Routed = running + hibernated: the gateway must route requests for a
	// hibernated sandbox here so this host can wake it. Only running ones
	// consume slots.
	routed, free, err := s.reg.RoutedCapacity(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: snapshot routed capacity: %v\n", err)
		return
	}
	warmReady, err := s.reg.WarmCount(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: count ready pool: %v\n", err)
		warmReady = 0
	}
	ids := make([]string, len(routed))
	runningCount, hibernated := 0, 0
	for i, sb := range routed {
		ids[i] = sb.ID
		if sb.Status == registry.StatusHibernated {
			hibernated++
		} else {
			runningCount++
		}
	}
	var snapIDs []string
	// One query for the whole host's public routes, not one per routed sandbox.
	// The per-sandbox loop this replaces made every heartbeat cost O(N)
	// registry round trips, so a host's control-plane overhead grew with its
	// inventory and (before the registry's reader/writer split) sat in front of
	// creates every 5 s. Filter against the routed ids we just read so we never
	// advertise a route for a sandbox absent from SandboxIDs — the gateway
	// would keep a route it cannot resolve to a host.
	var rawRoutes []cluster.RawPortRoute
	if routes, err := s.reg.PublicRoutes(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: list public routes: %v\n", err)
	} else {
		routedIDs := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			routedIDs[id] = struct{}{}
		}
		for _, pr := range routes {
			if _, ok := routedIDs[pr.SandboxID]; !ok {
				continue
			}
			rawRoutes = append(rawRoutes, cluster.RawPortRoute{
				PublicPort: pr.PublicPort, SandboxID: pr.SandboxID, GuestPort: pr.GuestPort,
			})
		}
	}
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
		Release:     s.cfg.WorkerRelease,
		SlotsTotal:  s.reg.Pools().Slots(),
		SlotsUsed:   runningCount,
		WarmReady:   warmReady,
		Hibernated:  hibernated,
		SandboxIDs:  ids,
		SnapshotIDs: snapIDs,
		RawRoutes:   rawRoutes,
	}
	if s.workerCredentials != nil {
		hb.ControlToken = s.workerCredentials.Outbound()
	}
	// Advertise true allocatable capacity. Memory overrides can make
	// SlotsTotal-SlotsUsed overstate it. Until the golden snapshot is ready,
	// advertise 0 — a fresh host that
	// invites a burst before it can hot-create serves nothing but cold-boot
	// storms and agent timeouts. RoutedCapacity calculated this value from the
	// same SQLite snapshot as SandboxIDs/SlotsUsed, so concurrent destroys
	// cannot combine an older used count with newer free capacity.
	hb.SlotsFree = intPtr(s.advertisedFreeSlots(free))
	b, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: build request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.gatewayCredentials != nil {
		req.Header.Set("Authorization", "Bearer "+s.gatewayCredentials.Outbound())
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: post to gateway: %v\n", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "heartbeat: gateway returned %s\n", resp.Status)
		return
	}

	// Boot-phase stamps (see bootphase.go). first_heartbeat_ok is when the
	// gateway can ROUTE to this host; capacity_advertised is when it can PLACE
	// on it — the two differ by golden warm-up and placement quarantine.
	s.phases.mark(phaseFirstHeartbeat)
	if hb.SlotsFree != nil && *hb.SlotsFree > 0 {
		s.phases.mark(phaseCapacityAdv)
	}
}

func (s *Server) advertisedFreeSlots(free int) int {
	select {
	case <-s.warmed:
	default:
		return 0
	}
	select {
	case <-s.readyPoolSettled:
	default:
		return 0
	}
	if s.cfg.PlacementDelay <= 0 {
		return free
	}
	age, err := s.bootAge()
	if err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: read boot age for placement gate: %v\n", err)
		return 0
	}
	if age < s.cfg.PlacementDelay {
		return 0
	}
	return free
}

func linuxBootAge() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/uptime is empty")
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/uptime %q: %w", fields[0], err)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("parse /proc/uptime %q: negative boot age", fields[0])
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func intPtr(v int) *int { return &v }
