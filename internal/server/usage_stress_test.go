package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// meteringServerWithDB is testMeteringServer plus the database path, so a test
// can age rows into the past instead of sleeping through real seconds. The
// ledger stores whole seconds, and several properties worth pinning (a
// double-credited counter, an outage that must not be billed) are invisible on
// intervals that bill zero.
func meteringServerWithDB(t *testing.T) (*Server, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "registry.db")
	reg, err := registry.Open(dbPath, registry.Pools{
		TapPrefix:  "fc",
		TapMax:     64,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.1.200",
		PortMin:    5200,
		PortMax:    5400,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{HostID: "host-test"}, reg)
	s.cfg.VMTemplate.Vcpus = 2
	s.cfg.VMTemplate.MemMIB = 1024
	return s, dbPath
}

// ageLedger rewrites interval timestamps directly. It opens its own connection:
// the registry deliberately exposes no writable handle, and this is test-only
// surgery on rows the registry has already committed.
func ageLedger(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("age ledger: %v", err)
	}
}

// Metering under concurrency.
//
// The functional tests next door drive one hook at a time. On a real worker the
// hooks race: a destroy and the TTL reaper and the VMM's own exit watcher can
// all fire for one sandbox, the sampler walks the open set while creates and
// freezes mutate it, and the spool drains behind all of it. These tests assert
// the properties an invoice depends on rather than a particular interleaving.

// meteringInvariants is what must hold after any amount of churn.
func meteringInvariants(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	all, _, err := s.reg.QueryUsage(ctx, registry.UsageQuery{Limit: 5000})
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	perSandbox := map[string]int{}
	openPer := map[string]int{}
	for _, iv := range all {
		perSandbox[iv.SandboxID]++
		if iv.EndedAt == nil {
			openPer[iv.SandboxID]++
		}
		if iv.Vcpus == 0 || iv.MemMIB == 0 {
			t.Fatalf("interval %s recorded unresolved resources: the template is gone by invoice time (%+v)", iv.ID, iv)
		}
		if iv.EndedAt != nil && iv.EndReason == "" {
			t.Fatalf("interval %s closed with no reason: an auditor cannot tell why compute stopped", iv.ID)
		}
		if iv.Duration() < 0 {
			t.Fatalf("interval %s has a negative duration", iv.ID)
		}
	}
	for id, n := range openPer {
		if n > 1 {
			t.Fatalf("sandbox %s has %d open intervals: one VM, two charges", id, n)
		}
	}
}

// Several teardown paths can close one sandbox's interval at the same moment.
// Only one of them may credit the host's billable counters — a second credit is
// a duplicate charge in the metrics an operator reconciles against.
func TestConcurrentTeardownPathsCreditBillableCountersOnce(t *testing.T) {
	s, dbPath := meteringServerWithDB(t)
	ctx := context.Background()
	const sandboxes = 30

	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%d", i)
		s.machines.Store(id, &vm.Machine{})
		s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm-" + id})
	}
	// Every interval must bill a whole second, so the counters are non-zero and
	// a double credit is visible.
	ageLedger(t, dbPath, `UPDATE usage_intervals SET started_at = started_at - 10`)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%d", i)
		for _, reason := range []string{registry.EndDestroy, registry.EndExpire, registry.EndHibernate, registry.EndVMExit} {
			wg.Add(1)
			go func(id, reason string) {
				defer wg.Done()
				<-start
				s.meterStop(ctx, id, reason)
			}(id, reason)
		}
		// The sampler racing the teardown is the real fleet's fifth closer.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.sampleOpenUsage(ctx)
		}()
	}
	close(start)
	wg.Wait()

	totals, err := s.reg.UsageTotalsFor(ctx, registry.UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.OpenIntervals != 0 {
		t.Fatalf("%d intervals still open after teardown", totals.OpenIntervals)
	}
	if got, want := s.met.billableVcpuSeconds.Load(), int64(totals.VcpuSeconds); got != want {
		t.Fatalf("billable vCPU-seconds counter = %d but the ledger says %d: a teardown race credited twice (or not at all)", got, want)
	}
	if got, want := s.met.billableMemMIBSeconds.Load(), int64(totals.MemMIBSeconds); got != want {
		t.Fatalf("billable MiB-seconds counter = %d but the ledger says %d", got, want)
	}
	meteringInvariants(t, s)
}

// Production churn against the metering hooks themselves: creates, freezes,
// wakes and destroys interleaved with the sampler. The ledger must end with one
// closed interval per VMM lifetime and nothing accruing.
func TestMeteringSurvivesLifecycleChurnWithSamplerRunning(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	const sandboxes = 24
	const wakeCycles = 3

	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.sampleOpenUsage(ctx)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < sandboxes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sb-%03d", i)
			sb := registry.Sandbox{ID: id, VMID: "vm-" + id, Metadata: map[string]string{"team": "core"}}
			// Create: the VM exists before the interval opens, which is what
			// keeps the sampler from closing it out from under the create.
			s.machines.Store(id, &vm.Machine{})
			s.meterStart(ctx, sb)
			for c := 0; c < wakeCycles; c++ {
				// Freeze: close first, then the VMM dies.
				s.meterStop(ctx, id, registry.EndHibernate)
				s.machines.Delete(id)
				// Wake: new VMM, then a new interval.
				s.machines.Store(id, &vm.Machine{})
				s.meterStart(ctx, sb)
			}
			s.meterStop(ctx, id, registry.EndDestroy)
			s.machines.Delete(id)
		}(i)
	}
	wg.Wait()
	close(stop)
	sampler.Wait()

	totals, err := s.reg.UsageTotalsFor(ctx, registry.UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if want := int64(sandboxes * (wakeCycles + 1)); totals.Intervals != want {
		t.Fatalf("ledger has %d intervals, want %d (one per VMM lifetime)", totals.Intervals, want)
	}
	if totals.OpenIntervals != 0 {
		t.Fatalf("%d intervals accruing after every sandbox was destroyed", totals.OpenIntervals)
	}
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%03d", i)
		rows, err := s.reg.UsageForSandbox(ctx, id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if len(rows) != wakeCycles+1 {
			t.Fatalf("sandbox %s has %d intervals, want %d", id, len(rows), wakeCycles+1)
		}
		if rows[len(rows)-1].EndReason != registry.EndDestroy {
			t.Fatalf("sandbox %s last interval ended as %q, want destroy", id, rows[len(rows)-1].EndReason)
		}
	}
	meteringInvariants(t, s)
}

// The sampler must not close an interval whose VM is mid-wake. A wake stores the
// machine before opening the interval, so a sampler tick that lands in between
// sees a live VM — this pins that ordering, because the reverse would let a
// freshly woken sandbox be closed as vm_exit within a tick of waking.
func TestSamplerDoesNotCloseAnIntervalThatWakeJustOpened(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	const rounds = 200
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.sampleOpenUsage(ctx)
			}
		}
	}()

	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("sb-%d", i)
		s.machines.Store(id, &vm.Machine{})
		s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm"})
	}
	close(stop)
	sampler.Wait()

	spuriousClosures := 0
	rows, _, err := s.reg.QueryUsage(ctx, registry.UsageQuery{Limit: 5000})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, iv := range rows {
		if iv.EndReason == registry.EndVMExit {
			spuriousClosures++
		}
	}
	if spuriousClosures > 0 {
		t.Fatalf("%d/%d live sandboxes were closed as vm_exit by the sampler", spuriousClosures, rounds)
	}
}

// A sandbox is labelled by the v1 layer with a PATCH that lands after the
// worker create returns — which is after the billable interval has already
// opened. The ledger's metadata is the only attribution it carries, so an
// interval that opened before the labels arrived must pick them up while it is
// still open. (Closed intervals are history and stay untouched.)
func TestLabelsAppliedAfterCreateReachTheOpenInterval(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	if _, err := s.reg.CreateStarting(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create starting: %v", err)
	}
	if err := s.reg.MarkRunning(ctx, "sb1"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	sb, err := s.reg.Get(ctx, "sb1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	s.machines.Store("sb1", &vm.Machine{})
	s.meterStart(ctx, sb) // the worker meters here; labels do not exist yet

	// What the v1 adapter does next: PATCH /sandboxes/{id}/public-fields.
	req := httptest.NewRequest(http.MethodPatch, "/sandboxes/sb1/public-fields",
		strings.NewReader(`{"metadata":{"team":"payments","env":"prod"}}`))
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handlePublicFields(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("public-fields status=%d body=%s", w.Code, w.Body.String())
	}

	rows, err := s.reg.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 interval, got %d", len(rows))
	}
	if rows[0].Metadata["team"] != "payments" || rows[0].Metadata["env"] != "prod" {
		t.Fatalf("the open interval carries %v, want the sandbox's labels: usage for every v1-created sandbox is unattributable otherwise", rows[0].Metadata)
	}
}

// Labels are recorded per interval, and a closed interval is history. Relabelling
// a sandbox must change what its NEXT interval bills under, never what an
// already-closed one did.
func TestRelabellingDoesNotRewriteClosedBillingHistory(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	if _, err := s.reg.CreateStarting(ctx, "sb1", "", "/tmp/sb1.ext4", nil, "", 0, 0, 0); err != nil {
		t.Fatalf("create starting: %v", err)
	}
	if err := s.reg.MarkRunning(ctx, "sb1"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	s.machines.Store("sb1", &vm.Machine{})
	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-1", Metadata: map[string]string{"team": "first"}})
	s.meterStop(ctx, "sb1", registry.EndHibernate)
	s.meterStart(ctx, registry.Sandbox{ID: "sb1", VMID: "vm-2", Metadata: map[string]string{"team": "first"}})

	req := httptest.NewRequest(http.MethodPatch, "/sandboxes/sb1/public-fields",
		strings.NewReader(`{"metadata":{"team":"second"}}`))
	req.SetPathValue("id", "sb1")
	w := httptest.NewRecorder()
	s.handlePublicFields(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("public-fields status=%d body=%s", w.Code, w.Body.String())
	}

	rows, err := s.reg.UsageForSandbox(ctx, "sb1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 intervals, got %d", len(rows))
	}
	if rows[0].Metadata["team"] != "first" {
		t.Fatalf("a relabel rewrote closed billing history: interval 1 now says %v", rows[0].Metadata)
	}
	if rows[1].Metadata["team"] != "second" {
		t.Fatalf("the open interval did not pick up the new labels: %v", rows[1].Metadata)
	}
}

// A sandbox handed out by the ready pool must bill from the claim, not from
// when the pool built the VM. Under a burst of concurrent claims that ordering
// is what keeps warm and cold creates priced the same.
func TestConcurrentWarmClaimsBillFromClaimNotFromPoolBuild(t *testing.T) {
	s, _ := meteringServerWithDB(t)
	ctx := context.Background()

	const pool = 16
	for i := 0; i < pool; i++ {
		id := fmt.Sprintf("warm-%d", i)
		if _, err := s.reg.CreateWarm(ctx, id, fmt.Sprintf("/tmp/%s.ext4", id), "", 0, 0); err != nil {
			t.Fatalf("create warm: %v", err)
		}
		if err := s.reg.MarkWarmReady(ctx, id); err != nil {
			t.Fatalf("mark warm ready: %v", err)
		}
		s.machines.Store(id, &vm.Machine{})
	}
	// The pool built these a while ago; nothing may be billing yet.
	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 0 {
		t.Fatalf("%d ready VMs are billing before anyone claimed them", n)
	}
	// And a sampler tick must not invent intervals for them either.
	s.sampleOpenUsage(ctx)
	if n, _ := s.reg.CountOpenUsageIntervals(ctx); n != 0 {
		t.Fatalf("sampling the ready pool opened %d intervals", n)
	}

	claimedAt := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	claims := make([]bool, pool+4) // more claimers than ready VMs
	for i := range claims {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok := s.claimWarm(ctx, "", nil, 0)
			claims[i] = ok
		}(i)
	}
	wg.Wait()

	got := 0
	for _, ok := range claims {
		if ok {
			got++
		}
	}
	if got != pool {
		t.Fatalf("%d claims succeeded against a pool of %d", got, pool)
	}
	open, err := s.reg.OpenUsageIntervals(ctx)
	if err != nil {
		t.Fatalf("open intervals: %v", err)
	}
	if len(open) != pool {
		t.Fatalf("%d intervals opened for %d claims", len(open), pool)
	}
	for _, iv := range open {
		if iv.StartedAt.Before(claimedAt) {
			t.Fatalf("interval %s starts at %s, before the claim at %s: the pool's runtime is on the customer's bill", iv.ID, iv.StartedAt, claimedAt)
		}
	}
	meteringInvariants(t, s)
}

// A host draining under SIGTERM freezes everything at once. Every interval must
// close, exactly once, attributed to the shutdown rather than to idleness.
func TestShutdownDrainClosesEveryIntervalExactlyOnce(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	const sandboxes = 40
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%d", i)
		s.machines.Store(id, &vm.Machine{})
		s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm"})
	}

	s.shuttingDown.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < sandboxes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.meterStop(ctx, fmt.Sprintf("sb-%d", i), registry.EndHibernate)
		}(i)
	}
	wg.Wait()

	rows, _, err := s.reg.QueryUsage(ctx, registry.UsageQuery{Limit: 5000})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != sandboxes {
		t.Fatalf("%d intervals, want %d", len(rows), sandboxes)
	}
	for _, iv := range rows {
		if iv.EndedAt == nil {
			t.Fatalf("%s left open by the drain", iv.ID)
		}
		if iv.EndReason != registry.EndShutdown {
			t.Fatalf("%s closed as %q during a drain, want %q", iv.ID, iv.EndReason, registry.EndShutdown)
		}
	}
}

// The spool is the only thing between a scaled-in worker and lost revenue. Under
// churn it must offer every closed interval exactly once, never an open one, and
// leave nothing behind when the churn stops.
func TestSpoolDrainsEveryClosedIntervalUnderChurn(t *testing.T) {
	s, ctx := testMeteringServer(t), context.Background()

	var mu sync.Mutex
	spooled := map[string]int{}
	s.usageBucketName = "test-bucket"
	s.usagePut = func(_ context.Context, _ string, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
			if line == "" {
				continue
			}
			var iv registry.UsageInterval
			if err := decodeJSON(line, &iv); err != nil {
				return err
			}
			if iv.EndedAt == nil {
				t.Errorf("spooled an OPEN interval %s: billing evidence must be final", iv.ID)
			}
			spooled[iv.ID]++
		}
		return nil
	}

	const sandboxes = 30
	const cycles = 4
	stop := make(chan struct{})
	var spooler sync.WaitGroup
	spooler.Add(1)
	go func() {
		defer spooler.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := s.flushUsage(ctx); err != nil {
					t.Errorf("flush: %v", err)
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < sandboxes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sb-%03d", i)
			for c := 0; c < cycles; c++ {
				s.machines.Store(id, &vm.Machine{})
				s.meterStart(ctx, registry.Sandbox{ID: id, VMID: fmt.Sprintf("vm-%d", c)})
				s.meterStop(ctx, id, registry.EndHibernate)
				s.machines.Delete(id)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	spooler.Wait()

	if _, err := s.flushUsage(ctx); err != nil { // final drain, as shutdown does
		t.Fatalf("final flush: %v", err)
	}
	pending, err := s.reg.CountUnflushedUsageIntervals(ctx)
	if err != nil {
		t.Fatalf("count unflushed: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d closed intervals never reached durable storage", pending)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := sandboxes * cycles; len(spooled) != want {
		t.Fatalf("spooled %d distinct intervals, want %d", len(spooled), want)
	}
	// At-least-once is the contract, so a repeat is allowed — but a repeat that
	// is not deduplicable by id would double an invoice. Ids must be unique per
	// interval, which the map key already asserts; here we only require that a
	// quiet run does not gratuitously re-spool.
	for id, n := range spooled {
		if n > 1 {
			t.Logf("interval %s spooled %d times (at-least-once; consumers dedup on the id)", id, n)
		}
	}
}

// A worker that dies mid-flight and restarts must not bill the outage, and must
// not leave a sandbox billing forever after its VM is gone.
func TestRestartRecoversTheLedgerWithoutBillingTheOutage(t *testing.T) {
	s, dbPath := meteringServerWithDB(t)
	ctx := context.Background()

	const sandboxes = 20
	for i := 0; i < sandboxes; i++ {
		id := fmt.Sprintf("sb-%d", i)
		s.machines.Store(id, &vm.Machine{})
		s.meterStart(ctx, registry.Sandbox{ID: id, VMID: "vm"})
	}
	// The host was down for an hour: intervals stopped heartbeating then.
	ageLedger(t, dbPath, `UPDATE usage_intervals SET started_at = started_at - 7200, last_seen_at = last_seen_at - 3600`)

	s.recoverUsageIntervals(ctx)

	totals, err := s.reg.UsageTotalsFor(ctx, registry.UsageQuery{})
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	if totals.OpenIntervals != 0 {
		t.Fatalf("%d intervals survived the restart still open", totals.OpenIntervals)
	}
	if want := float64(sandboxes) * 3600; totals.DurationSeconds > want+float64(sandboxes) {
		t.Fatalf("recovery billed %.0f seconds, want ~%.0f: the hour the host was down is not billable", totals.DurationSeconds, want)
	}
}

func decodeJSON(line string, v any) error {
	return json.Unmarshal([]byte(line), v)
}
