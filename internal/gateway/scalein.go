package gateway

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Scale-in belongs to the gateway for the same reason scale-out does: it is the
// only component that knows what a host is actually holding.
//
// Nomad cannot know. Every worker runs ONE system-job allocation whether it
// holds zero sandboxes or fifty, because sandboxes are Firecracker VMs inside
// that allocation rather than allocations themselves. So the autoscaler's
// node_selector_strategy is a guess about occupancy made by something that
// cannot observe occupancy, and node_purge acts on the guess by destroying the
// data disk. Measured 2026-08-16: it selected the host the gateway had just
// added for a burst and killed 11 running trials
// (`502 host ... unreachable: EOF`).
//
// The controller therefore NEVER resizes the group down. It cordons a host,
// waits for it to empty, and then deletes that specific instance by name. A
// sandbox is never destroyed to make a scale-in happen; a scale-in happens
// because a host became empty. Every step before the delete is reversible, so
// demand returning mid-drain costs nothing but a cordon.
const (
	// scaleInInterval paces the controller. Scale-in is deliberately slow: the
	// cost of being one host late is money, and the cost of being one host early
	// is someone's work.
	scaleInInterval = 30 * time.Second
	// defaultScaleInAfter is how long demand must stay below the fleet size
	// before a cordon. It is long because occupancy here is long-lived: a
	// sandbox can idle for minutes between execs without being garbage.
	defaultScaleInAfter = 10 * time.Minute
)

// InstanceRemover is an optional DirectScaler capability: remove ONE named
// instance from the group. Optional rather than part of DirectScaler so a
// deployment (or a test fake) without it keeps working — the gateway then
// cordons and drains but never removes, which is a cost, not a hazard.
type InstanceRemover interface {
	DeleteInstance(context.Context, string) error
}

// ConfigureScaleIn enables gateway-owned scale-in. min is the floor the fleet
// may never shrink past; after is how long demand must stay low before a host
// is cordoned (0 uses defaultScaleInAfter). Requires ConfigureDirectScaleOut to
// have run, since both directions read the same demand function — that shared
// arithmetic is the point, and is what two independent controllers could not
// have.
func (g *Gateway) ConfigureScaleIn(min int, after time.Duration) error {
	if g.directScaler == nil {
		return fmt.Errorf("scale-in requires direct scaling to be configured")
	}
	if min < 1 {
		return fmt.Errorf("scale-in minimum must be at least 1")
	}
	if after <= 0 {
		after = defaultScaleInAfter
	}
	g.scaleInMin = min
	g.scaleInAfter = after
	g.scaleInEnabled = true
	return nil
}

func (g *Gateway) scaleInLoop(ctx context.Context) {
	if !g.scaleInEnabled {
		return
	}
	ticker := time.NewTicker(scaleInInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.evaluateScaleIn(ctx)
		}
	}
}

// evaluateScaleIn runs one pass: honour returning demand, retire whatever
// finished draining, then decide whether to cordon one more host.
func (g *Gateway) evaluateScaleIn(ctx context.Context) {
	fs := g.fleetDemand()

	// Demand is checked FIRST, before retiring anything. A drained host and a
	// returning burst can land in the same pass, and deleting an empty host we
	// are about to need means paying a full boot to get back what we already
	// had. Uncordoning is free, so the tie goes to keeping the capacity.
	if fs.queued > 0 || fs.demand >= fs.live {
		if n := g.uncordonAll(); n > 0 {
			fmt.Fprintf(os.Stderr, "gateway: scale-in aborted, uncordoned %d host(s) (demand=%d live=%d queued=%d)\n",
				n, fs.demand, fs.live, fs.queued)
			g.scaleInAborted.Add(uint64(n))
		}
		g.scaleInLowSince = time.Time{}
		return
	}

	// Demand is genuinely low, so anything that finished draining can go.
	g.retireDrainedHosts(ctx)

	// Require demand to stay low for a full window. Without this a single quiet
	// scrape between two waves of a burst is enough to start draining a host the
	// next wave immediately needs.
	now := time.Now()
	if g.scaleInLowSince.IsZero() {
		g.scaleInLowSince = now
		return
	}
	if now.Sub(g.scaleInLowSince) < g.scaleInAfter {
		return
	}

	// One drain at a time. Cordoning several hosts at once concentrates the
	// remaining load onto fewer hosts while none of them have actually emptied,
	// which is how a fleet talks itself into a capacity cliff.
	if g.hasDrainingHost() {
		return
	}
	if fs.live <= g.scaleInMin {
		return
	}

	victim := g.cordonLeastLoaded()
	if victim == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "gateway: cordoned host %s for scale-in (demand=%d live=%d)\n", victim, fs.demand, fs.live)
	g.scaleInCordons.Add(1)
	// Reset the window so the NEXT cordon has to earn its own quiet period
	// rather than following immediately behind this one.
	g.scaleInLowSince = time.Time{}
}

// cordonLeastLoaded marks the emptiest eligible host as draining and returns its
// id, or "" if nothing is eligible.
func (g *Gateway) cordonLeastLoaded() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	var best *host
	for _, h := range g.hosts {
		// A host the provider cannot be told to delete is not a scale-in
		// candidate: draining it would strand it cordoned forever.
		if h.instanceName == "" || h.draining || now.Sub(h.lastSeen) > g.ttl {
			continue
		}
		if best == nil || h.load() < best.load() || (h.load() == best.load() && h.id < best.id) {
			best = h
		}
	}
	if best == nil {
		return ""
	}
	best.draining = true
	best.drainingSince = now
	return best.id
}

// retireDrainedHosts deletes the provider instance behind every cordoned host
// that has finished emptying.
func (g *Gateway) retireDrainedHosts(ctx context.Context) {
	remover, ok := g.directScaler.(InstanceRemover)
	if !ok {
		return
	}
	type target struct{ id, instance string }
	var drained []target

	g.mu.RLock()
	now := time.Now()
	for _, h := range g.hosts {
		if !h.draining || h.instanceName == "" {
			continue
		}
		// Still heartbeating is a precondition, not a detail: a silent host's
		// counts are stale, and "looks empty" would then mean "we stopped
		// hearing about its sandboxes".
		if now.Sub(h.lastSeen) > g.ttl {
			continue
		}
		if h.load() > 0 {
			continue
		}
		drained = append(drained, target{id: h.id, instance: h.instanceName})
	}
	g.mu.RUnlock()

	for _, t := range drained {
		// Re-check the floor per removal: several hosts can finish draining
		// between passes, and the fleet must not step under min on the way.
		if g.liveHostCount() <= g.scaleInMin {
			return
		}
		delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := remover.DeleteInstance(delCtx, t.instance)
		cancel()
		if err != nil {
			// Leave it cordoned and try again next pass. It holds nothing, so a
			// failed delete costs money, not correctness.
			fmt.Fprintf(os.Stderr, "gateway: scale-in delete of %s failed: %v\n", t.instance, err)
			g.scaleInFailed.Add(1)
			continue
		}
		fmt.Fprintf(os.Stderr, "gateway: scale-in removed drained host %s (instance %s)\n", t.id, t.instance)
		g.scaleInRemoved.Add(1)
		g.mu.Lock()
		g.dropHostLocked(t.id)
		g.mu.Unlock()
		// The provider target just changed; refresh so the exported size and the
		// scale-out watermark do not carry a stale value for a poll interval.
		if sizer, ok := g.directScaler.(TargetSizer); ok {
			g.refreshMIGTarget(ctx, sizer)
		}
	}
}

// uncordonAll clears every cordon and reports how many it cleared.
func (g *Gateway) uncordonAll() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, h := range g.hosts {
		if h.draining {
			h.draining = false
			h.drainingSince = time.Time{}
			n++
		}
	}
	return n
}

func (g *Gateway) hasDrainingHost() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, h := range g.hosts {
		if h.draining {
			return true
		}
	}
	return false
}

func (g *Gateway) liveHostCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, h := range g.hosts {
		if now.Sub(h.lastSeen) <= g.ttl {
			n++
		}
	}
	return n
}

func (g *Gateway) drainingHostCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n := 0
	for _, h := range g.hosts {
		if h.draining {
			n++
		}
	}
	return n
}
