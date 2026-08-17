package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeScaler records what the gateway asked the provider to do.
type fakeScaler struct {
	mu      sync.Mutex
	deleted []string
	target  int
	failDel error
}

func (f *fakeScaler) ScaleOut(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = n
	return nil
}

func (f *fakeScaler) TargetSize(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, nil
}

func (f *fakeScaler) DeleteInstance(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDel != nil {
		return f.failDel
	}
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeScaler) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// scaleInGateway builds a gateway with scale-in armed and the demand arithmetic
// configured so one host holds 10 slots.
func scaleInGateway(t *testing.T, min int, hosts ...*host) (*Gateway, *fakeScaler) {
	t.Helper()
	g := liveGateway(hosts...)
	f := &fakeScaler{target: len(hosts)}
	if err := g.ConfigureDirectScaleOut(f, 10, 0); err != nil {
		t.Fatal(err)
	}
	if err := g.ConfigureScaleIn(min, time.Minute); err != nil {
		t.Fatal(err)
	}
	return g, f
}

// A cordoned host keeps serving what it holds but must attract nothing new.
// If any placement path ignored the cordon it would refill the host through one
// door while the controller drained it through another, and the drain would
// never finish.
func TestCordonedHostTakesNoNewPlacements(t *testing.T) {
	g := liveGateway(
		&host{id: "keep", slotsTotal: 10, slotsUsed: 1, slotsFree: 9},
		&host{id: "drain", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
	)
	g.hosts["drain"].draining = true

	// Ordinary placement bin-packs onto the fullest host, so without the cordon
	// "keep" wins anyway. Force the opposite: make "drain" the tighter fit.
	g.hosts["keep"].slotsFree = 10
	g.hosts["drain"].slotsFree = 1

	for _, name := range []string{"reserveHost", "reserveHostOrdinary"} {
		var got *host
		if name == "reserveHost" {
			got = g.reserveHost(nil)
		} else {
			got = g.reserveHostOrdinary(nil)
		}
		if got == nil {
			t.Fatalf("%s: expected a placement", name)
		}
		if got.id == "drain" {
			t.Errorf("%s placed on a cordoned host", name)
		}
		g.releaseReservation(got, false)
	}

	// The snapshot path is a separate selector and must honour the cordon too —
	// including when the cordoned host is the snapshot's preferred owner.
	got := g.reserveHostFor(nil, 1, "drain")
	if got == nil {
		t.Fatal("reserveHostFor: expected a placement")
	}
	if got.id == "drain" {
		t.Error("reserveHostFor placed on a cordoned host despite the cordon")
	}
}

// Scale-in must not fire on a single quiet moment between two waves of a burst.
func TestScaleInRequiresSustainedLowDemand(t *testing.T) {
	g, _ := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 1, slotsFree: 9},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
	)
	ctx := context.Background()

	g.evaluateScaleIn(ctx) // starts the clock, cordons nothing
	if n := g.drainingHostCount(); n != 0 {
		t.Fatalf("cordoned %d hosts on the first low reading, want 0", n)
	}
	g.evaluateScaleIn(ctx) // still inside the window
	if n := g.drainingHostCount(); n != 0 {
		t.Fatalf("cordoned %d hosts before the window elapsed, want 0", n)
	}

	g.scaleInLowSince = time.Now().Add(-2 * time.Minute) // window satisfied
	g.evaluateScaleIn(ctx)
	if n := g.drainingHostCount(); n != 1 {
		t.Fatalf("cordoned %d hosts after the window, want 1", n)
	}
}

// Once an uninterrupted low-demand period has earned the quiet window, each
// drained removal may advance directly to the next single-host cordon. Making
// every host re-earn the whole window turns an N-host correction into N times
// scaleInAfter even though demand is checked before every delete and cordon.
func TestScaleInRetiresAndCordonsNextDuringSameLowDemandPeriod(t *testing.T) {
	g, f := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsFree: 10},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsFree: 10},
		&host{id: "c", instanceName: "vm-c", slotsTotal: 10, slotsFree: 10},
	)
	g.scaleInLowSince = time.Now().Add(-2 * time.Minute)
	g.evaluateScaleIn(context.Background())
	if got := g.drainingHostCount(); got != 1 {
		t.Fatalf("first pass draining hosts = %d, want 1", got)
	}

	g.evaluateScaleIn(context.Background())
	if names := f.deletedNames(); len(names) != 1 {
		t.Fatalf("second pass deleted %v, want one drained host", names)
	}
	if got := g.drainingHostCount(); got != 1 {
		t.Fatalf("second pass draining hosts = %d, want next single host", got)
	}
	if g.scaleInLowSince.IsZero() {
		t.Fatal("continuous low demand unexpectedly reset the quiet-period timestamp")
	}
}

// The victim is the emptiest host, and one that the provider can actually be
// told to delete.
func TestScaleInCordonsEmptiestNameableHost(t *testing.T) {
	g, _ := scaleInGateway(t, 1,
		&host{id: "busy", instanceName: "vm-busy", slotsTotal: 10, slotsUsed: 5, slotsFree: 5},
		&host{id: "idle", instanceName: "vm-idle", slotsTotal: 10, slotsUsed: 1, slotsFree: 9},
	)
	g.scaleInLowSince = time.Now().Add(-2 * time.Minute)
	g.evaluateScaleIn(context.Background())

	if !g.hosts["idle"].draining {
		t.Error("expected the emptiest host to be cordoned")
	}
	if g.hosts["busy"].draining {
		t.Error("cordoned the busier host")
	}
}

// A host the gateway cannot name to the provider must never be cordoned:
// draining it would strand it cordoned forever, since the delete can never be
// issued. Old workers and non-GCE hosts report no instance name.
func TestScaleInSkipsHostWithoutInstanceName(t *testing.T) {
	g, f := scaleInGateway(t, 1,
		&host{id: "busy", instanceName: "vm-busy", slotsTotal: 10, slotsUsed: 5, slotsFree: 5},
		&host{id: "anon", slotsTotal: 10, slotsUsed: 0, slotsFree: 10}, // emptiest, unnameable
	)
	g.scaleInLowSince = time.Now().Add(-2 * time.Minute)
	g.evaluateScaleIn(context.Background())

	if g.hosts["anon"].draining {
		t.Error("cordoned a host with no instance name; it could never be removed")
	}
	if names := f.deletedNames(); len(names) != 0 {
		t.Errorf("deleted %v without an instance name", names)
	}
}

// Demand returning mid-drain must release the cordon. A cordoned host is
// capacity already paid for; reusing it beats booting a replacement.
func TestScaleInUncordonsWhenDemandReturns(t *testing.T) {
	g, _ := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 1, slotsFree: 9},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
	)
	ctx := context.Background()
	g.scaleInLowSince = time.Now().Add(-2 * time.Minute)
	g.evaluateScaleIn(ctx)
	if g.drainingHostCount() != 1 {
		t.Fatal("expected a cordon to set up the test")
	}

	g.queued.Store(5) // a burst arrives
	g.evaluateScaleIn(ctx)

	if n := g.drainingHostCount(); n != 0 {
		t.Fatalf("%d hosts still cordoned after demand returned", n)
	}
	if got := g.scaleInAborted.Load(); got != 1 {
		t.Errorf("scaleInAborted = %d, want 1", got)
	}
	if !g.scaleInLowSince.IsZero() {
		t.Error("the low-demand window should restart once demand returns")
	}
}

// The delete only happens once the host is actually empty — that is the whole
// safety property. A cordoned host still holding sandboxes must survive.
func TestScaleInDeletesOnlyDrainedHosts(t *testing.T) {
	g, f := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 4, slotsFree: 6},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 2, slotsFree: 8},
	)
	ctx := context.Background()
	g.hosts["b"].draining = true

	g.retireDrainedHosts(ctx)
	if names := f.deletedNames(); len(names) != 0 {
		t.Fatalf("deleted %v while the host still held sandboxes", names)
	}

	// Hibernated sandboxes live on THIS host's disk, so they block the drain
	// exactly as running ones do.
	g.hosts["b"].slotsUsed = 0
	g.hosts["b"].hibernated = 3
	g.retireDrainedHosts(ctx)
	if names := f.deletedNames(); len(names) != 0 {
		t.Fatalf("deleted %v with hibernated sandboxes still on disk", names)
	}

	// An in-flight create counts too: the reservation lands after the delete.
	g.hosts["b"].hibernated = 0
	g.hosts["b"].reserved = 1
	g.retireDrainedHosts(ctx)
	if names := f.deletedNames(); len(names) != 0 {
		t.Fatalf("deleted %v with a create in flight", names)
	}

	// Fully drained: now it goes, by name, and stops being routable.
	g.hosts["b"].reserved = 0
	g.retireDrainedHosts(ctx)
	names := f.deletedNames()
	if len(names) != 1 || names[0] != "vm-b" {
		t.Fatalf("deleted %v, want [vm-b]", names)
	}
	if _, ok := g.hosts["b"]; ok {
		t.Error("removed host is still in the routing table")
	}
}

// Ready-pool VMs hold no user state and are refilled by the worker, so waiting
// for them would mean waiting forever.
func TestScaleInIgnoresReadyPoolWhenDraining(t *testing.T) {
	g, f := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 4, slotsFree: 6},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 0, warmReady: 8},
	)
	g.hosts["b"].draining = true
	g.retireDrainedHosts(context.Background())

	if names := f.deletedNames(); len(names) != 1 || names[0] != "vm-b" {
		t.Fatalf("deleted %v, want [vm-b]: a ready pool must not block a drain", names)
	}
}

// The floor is absolute: an empty fleet cannot serve the create that would grow
// it back.
func TestScaleInHoldsTheFloor(t *testing.T) {
	g, f := scaleInGateway(t, 2,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
	)
	ctx := context.Background()
	g.scaleInLowSince = time.Now().Add(-2 * time.Minute)
	g.evaluateScaleIn(ctx)
	if n := g.drainingHostCount(); n != 0 {
		t.Fatalf("cordoned %d hosts at the floor, want 0", n)
	}

	// Even an already-cordoned, fully drained host must not take the fleet under
	// the floor.
	g.hosts["b"].draining = true
	g.retireDrainedHosts(ctx)
	if names := f.deletedNames(); len(names) != 0 {
		t.Errorf("deleted %v, breaching the minimum fleet size", names)
	}
}

// A failed delete leaves the host cordoned for the next pass. It holds nothing,
// so this costs money, not correctness — but it must not vanish from routing.
func TestScaleInRetriesFailedDelete(t *testing.T) {
	g, f := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 4, slotsFree: 6},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 0, slotsFree: 10},
	)
	ctx := context.Background()
	g.hosts["b"].draining = true
	f.failDel = errors.New("quota exceeded")

	g.retireDrainedHosts(ctx)
	if _, ok := g.hosts["b"]; !ok {
		t.Fatal("host dropped from routing despite a failed delete")
	}
	if !g.hosts["b"].draining {
		t.Error("host lost its cordon after a failed delete")
	}
	if got := g.scaleInFailed.Load(); got != 1 {
		t.Errorf("scaleInFailed = %d, want 1", got)
	}

	f.failDel = nil
	g.retireDrainedHosts(ctx)
	if names := f.deletedNames(); len(names) != 1 || names[0] != "vm-b" {
		t.Fatalf("retry deleted %v, want [vm-b]", names)
	}
}

// Draining state has to be observable, or a host stuck behind a sandbox that
// never leaves is capacity silently paid for and never used.
func TestScaleInMetricsExposeDraining(t *testing.T) {
	g, _ := scaleInGateway(t, 1,
		&host{id: "a", instanceName: "vm-a", slotsTotal: 10, slotsUsed: 4, slotsFree: 6},
		&host{id: "b", instanceName: "vm-b", slotsTotal: 10, slotsUsed: 1, slotsFree: 9},
	)
	g.hosts["b"].draining = true
	body := gatewayMetrics(g)
	for _, want := range []string{
		"sandbox_hosts_draining 1",
		"sandbox_scale_in_cordons_total 0",
		"sandbox_scale_in_removed_total 0",
		"sandbox_scale_in_aborted_total 0",
		"sandbox_scale_in_failed_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, body)
		}
	}
}

// The gateway learns the instance name from heartbeats, and a heartbeat that
// momentarily omits it must not erase one already known — otherwise a worker
// whose metadata lookup blips becomes silently unremovable, possibly while it
// is mid-drain.
func TestInstanceNameLearnedAndNotErasedByHeartbeat(t *testing.T) {
	g := New("tok", 20*time.Second, 0, 0)
	post := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		g.handleRegister(rr, httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body)))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("register: got %d: %s", rr.Code, rr.Body.String())
		}
	}

	post(`{"host_id":"w","addr":"10.0.0.1:8080","instance_name":"vm-w","slots_total":4,"slots_used":0,"slots_free":4,"sandbox_ids":[]}`)
	if got := g.hosts["w"].instanceName; got != "vm-w" {
		t.Fatalf("instanceName = %q, want vm-w", got)
	}

	// Same worker, instance name absent (older release, or a metadata blip).
	post(`{"host_id":"w","addr":"10.0.0.1:8080","slots_total":4,"slots_used":0,"slots_free":4,"sandbox_ids":[]}`)
	if got := g.hosts["w"].instanceName; got != "vm-w" {
		t.Errorf("instanceName = %q after a heartbeat without one, want vm-w retained", got)
	}
}
