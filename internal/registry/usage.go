package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UsageInterval is one billable span: the time a single Firecracker process
// served a user-visible sandbox.
//
// The unit is deliberately the VMM lifetime rather than the sandbox lifetime.
// Every jailed VM gets its own cgroup leaf, created at launch and removed when
// the VMM exits, so a leaf's cpu.stat is scoped to exactly one interval — no
// cross-interval subtraction, and no way to attribute one sandbox's CPU to
// another. A hibernate/wake cycle therefore produces two intervals, which is
// also what we want to bill: the frozen span in between consumes neither CPU
// nor memory.
//
// Rows outlive the sandbox they describe. Destroy deletes the sandboxes row
// outright, so a ledger that joined against it would lose the usage of every
// terminated sandbox — which is all of them, eventually.
type UsageInterval struct {
	// ID is deterministic: "<host>:<sandbox>:<seq>". The durability spool is
	// at-least-once, so consumers dedup on this rather than on a random key.
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
	// Seq counts intervals for one sandbox, from 1. A wake opens Seq+1.
	Seq    int64  `json:"seq"`
	HostID string `json:"host_id"`
	VMID   string `json:"vm_id"`
	// Vcpus/MemMIB are EFFECTIVE resources, snapshotted at open. The registry
	// stores 0 for "template default" and the sandbox row is gone by invoice
	// time, so the resolved values are what we keep.
	Vcpus  int64 `json:"vcpus"`
	MemMIB int64 `json:"mem_mib"`
	// Metadata is the sandbox's labels while this interval was accruing. Not
	// an owner concept (there is no tenant model yet) — it is recorded so that
	// whatever callers label sandboxes with today stays recoverable when
	// attribution is designed.
	//
	// An OPEN interval tracks the sandbox's current labels (see
	// SetOpenUsageMetadata: the v1 API labels a sandbox just after the interval
	// opens, so a snapshot taken at open would be empty for every sandbox it
	// ever created). A CLOSED interval is history and is never rewritten.
	Metadata  map[string]string `json:"metadata,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	// EndedAt is nil while the interval is open.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// LastSeenAt is advanced by the sampler. It is the truncation point for an
	// interval left open by a crashed server: those seconds after it were not
	// served, so they are not billed.
	LastSeenAt time.Time `json:"last_seen_at"`
	// CPUUsec is host CPU time consumed DURING this interval, from the cgroup
	// leaf. Recorded but not billed — see docs/usage-metering-plan.md.
	CPUUsec int64 `json:"cpu_usec"`
	// CPUUsecBase is the leaf's reading when the interval opened, and CPUUsec
	// is measured from it.
	//
	// This is not bookkeeping for its own sake. A ready-pool VM is launched
	// minutes before anyone claims it, and its leaf accumulates boot and idle
	// CPU the whole time — so the absolute counter at claim already holds ~18
	// CPU-seconds that are platform overhead, not the customer's work.
	// Reporting it raw produced consumed-CPU figures ABOVE the physical
	// ceiling of the interval (18.15 CPU-s for a 5 s interval on a 2-vCPU
	// guest), which is both wrong and visibly wrong. Recorded rather than
	// subtracted-and-discarded so a row can still be audited back to the two
	// readings it came from.
	CPUUsecBase int64      `json:"cpu_usec_base"`
	EndReason   string     `json:"end_reason,omitempty"`
	FlushedAt   *time.Time `json:"-"`
}

// End reasons. Anything that closes an interval names itself, so a ledger row
// says why compute stopped without joining anything.
const (
	EndDestroy   = "destroy"
	EndHibernate = "hibernate"
	EndExpire    = "expire"
	EndShutdown  = "shutdown"
	EndVMExit    = "vm_exit"
	EndCrash     = "crash"
)

// Duration is the billable span: to EndedAt when closed, and only to
// LastSeenAt while open. An open interval must never be measured to "now" —
// after a crash that would bill an outage.
func (u UsageInterval) Duration() time.Duration {
	end := u.LastSeenAt
	if u.EndedAt != nil {
		end = *u.EndedAt
	}
	if end.Before(u.StartedAt) {
		return 0
	}
	return end.Sub(u.StartedAt)
}

// VcpuSeconds is the billed CPU quantity: allocated vCPUs × duration.
func (u UsageInterval) VcpuSeconds() float64 {
	return float64(u.Vcpus) * u.Duration().Seconds()
}

// MemMIBSeconds is the billed memory quantity: allocated MiB × duration.
func (u UsageInterval) MemMIBSeconds() float64 {
	return float64(u.MemMIB) * u.Duration().Seconds()
}

// CPUSeconds is consumed host CPU time. Recorded for margin analysis against
// the deliberate CPU oversubscription; not the billing base.
func (u UsageInterval) CPUSeconds() float64 {
	return float64(u.CPUUsec) / 1e6
}

func (r *Registry) migrateUsage() error {
	_, err := r.db.Exec(`
	CREATE TABLE IF NOT EXISTS usage_intervals (
		id           TEXT PRIMARY KEY,
		sandbox_id   TEXT NOT NULL,
		seq          INTEGER NOT NULL,
		host_id      TEXT NOT NULL,
		vm_id        TEXT NOT NULL,
		vcpus        INTEGER NOT NULL,
		mem_mib      INTEGER NOT NULL,
		metadata     TEXT NOT NULL DEFAULT '{}',
		started_at   INTEGER NOT NULL,
		ended_at     INTEGER,
		last_seen_at INTEGER NOT NULL,
		cpu_usec     INTEGER NOT NULL DEFAULT 0,
		cpu_usec_base INTEGER NOT NULL DEFAULT 0,
		end_reason   TEXT NOT NULL DEFAULT '',
		flushed_at   INTEGER
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_usage_seq ON usage_intervals(sandbox_id, seq);
	-- At most one OPEN interval per sandbox. A double-open is a billing bug
	-- (two concurrent charges for one VM), so the database refuses it rather
	-- than leaving it to be noticed on an invoice.
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_usage_open ON usage_intervals(sandbox_id) WHERE ended_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_usage_open ON usage_intervals(ended_at) WHERE ended_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_usage_flush ON usage_intervals(flushed_at) WHERE flushed_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_usage_started ON usage_intervals(started_at);
	-- Sequence numbers a sandbox already used on ANOTHER host. A sandbox that
	-- moves (host failure, adoption) arrives at a registry that has never seen
	-- it and would otherwise restart its numbering at 1 — colliding with the
	-- intervals its old host already billed, in a namespace the public API
	-- presents as "<sandbox>:<sequence>" and promises is unique.
	CREATE TABLE IF NOT EXISTS usage_seq_floor (
		sandbox_id TEXT PRIMARY KEY,
		seq        INTEGER NOT NULL
	);
	`)
	if err != nil {
		return err
	}
	// cpu_usec_base arrived after the ledger shipped. ALTER TABLE has no
	// IF NOT EXISTS, so the duplicate-column error is the "already migrated"
	// signal. Existing rows default to 0, which reproduces the old absolute
	// reading rather than inventing a correction for intervals nobody
	// measured a baseline for.
	if _, err := r.db.Exec(`ALTER TABLE usage_intervals ADD COLUMN cpu_usec_base INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// OpenUsageInterval starts billing a sandbox. Callers pass EFFECTIVE resources
// (template defaults already resolved) because the registry stores 0 to mean
// "default" and only the server knows the template.
//
// It is called when a sandbox becomes usable — MarkRunning, a warm claim, or a
// wake — never at create acceptance: bring-up latency and pre-claim ready-pool
// runtime are the platform's cost, not the customer's.
//
// cpuUsecBase is the VM's cgroup CPU reading at this moment; consumed CPU is
// reported relative to it. Pass a negative value when it cannot be read, which
// records 0 and degrades to the absolute counter. It matters most on the
// default create path: a ready-pool VM has been burning CPU since the pool
// built it, and that time is ours, not the customer's.
//
// Opening twice for one sandbox returns ErrUsageOpen instead of double-billing.
func (r *Registry) OpenUsageInterval(ctx context.Context, sandboxID, hostID, vmID string, vcpus, memMIB, cpuUsecBase int64, metadata map[string]string) (UsageInterval, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageInterval{}, err
	}
	defer tx.Rollback()

	// The next sequence number continues from whichever is higher: what this
	// host has already billed, or what the sandbox used before it moved here.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(seq) FROM (
			SELECT MAX(seq) AS seq FROM usage_intervals WHERE sandbox_id=?1
			UNION ALL
			SELECT seq FROM usage_seq_floor WHERE sandbox_id=?1
		)`, sandboxID).Scan(&maxSeq); err != nil {
		return UsageInterval{}, fmt.Errorf("read usage seq: %w", err)
	}

	meta, err := json.Marshal(nonNilMetadata(metadata))
	if err != nil {
		return UsageInterval{}, fmt.Errorf("encode usage metadata: %w", err)
	}

	if cpuUsecBase < 0 {
		cpuUsecBase = 0
	}
	now := time.Now().UTC().Truncate(time.Second)
	u := UsageInterval{
		ID:          usageIntervalID(hostID, sandboxID, maxSeq.Int64+1),
		SandboxID:   sandboxID,
		Seq:         maxSeq.Int64 + 1,
		HostID:      hostID,
		VMID:        vmID,
		Vcpus:       vcpus,
		MemMIB:      memMIB,
		Metadata:    metadata,
		StartedAt:   now,
		LastSeenAt:  now,
		CPUUsecBase: cpuUsecBase,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO usage_intervals (id, sandbox_id, seq, host_id, vm_id, vcpus, mem_mib, metadata, started_at, last_seen_at, cpu_usec_base)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.SandboxID, u.Seq, u.HostID, u.VMID, u.Vcpus, u.MemMIB, string(meta),
		u.StartedAt.Unix(), u.LastSeenAt.Unix(), u.CPUUsecBase); err != nil {
		if isUniqueViolation(err) {
			return UsageInterval{}, fmt.Errorf("%w: sandbox %s", ErrUsageOpen, sandboxID)
		}
		return UsageInterval{}, fmt.Errorf("insert usage interval: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UsageInterval{}, err
	}
	return u, nil
}

// TouchUsageInterval advances the open interval's heartbeat and consumed CPU.
// cpuUsec is the cgroup's absolute counter for this VMM, not a delta — the leaf
// lives exactly as long as the interval, so the latest reading IS the total.
//
// This is what bounds crash loss: a host that dies loses at most one sampling
// window of a bill rather than the whole interval.
func (r *Registry) TouchUsageInterval(ctx context.Context, sandboxID string, cpuUsec int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE usage_intervals
		    SET last_seen_at=?,
		        cpu_usec=CASE WHEN ? < 0 THEN cpu_usec ELSE MAX(cpu_usec, MAX(? - cpu_usec_base, 0)) END
		  WHERE sandbox_id=? AND ended_at IS NULL`,
		time.Now().UTC().Unix(), cpuUsec, cpuUsec, sandboxID)
	return err
}

// SetOpenUsageMetadata records a sandbox's current labels on the interval it is
// accruing right now. Closed intervals are never touched.
//
// This exists because labels arrive AFTER the interval opens. The v1 adapter
// creates a sandbox on a worker and only then PATCHes its metadata, so an
// interval that snapshotted labels at open would record an empty map for every
// sandbox the public API ever created — and metadata is the only attribution
// this ledger carries.
//
// Restricting it to the open interval is what keeps "a later PATCH cannot
// rewrite billing history" true: a closed interval is a fact, already spoolable
// and possibly already durable in the bucket, while an open one is still
// accruing and has not been reported to anyone as final.
func (r *Registry) SetOpenUsageMetadata(ctx context.Context, sandboxID string, metadata map[string]string) error {
	meta, err := json.Marshal(nonNilMetadata(metadata))
	if err != nil {
		return fmt.Errorf("encode usage metadata: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE usage_intervals SET metadata=? WHERE sandbox_id=? AND ended_at IS NULL`,
		string(meta), sandboxID)
	return err
}

// LastUsageSeq is the highest sequence number this sandbox has reached here,
// including anything it carried in from a previous host. Zero means it has
// never been billed. It is what travels with a sandbox that moves, so its
// numbering continues instead of restarting.
func (r *Registry) LastUsageSeq(ctx context.Context, sandboxID string) (int64, error) {
	var seq sql.NullInt64
	err := r.rdb.QueryRowContext(ctx, `
		SELECT MAX(seq) FROM (
			SELECT MAX(seq) AS seq FROM usage_intervals WHERE sandbox_id=?1
			UNION ALL
			SELECT seq FROM usage_seq_floor WHERE sandbox_id=?1
		)`, sandboxID).Scan(&seq)
	return seq.Int64, err
}

// SetUsageSeqFloor records that a sandbox already billed up to seq somewhere
// else, so this host's first interval for it is seq+1.
//
// Without it, an adopted sandbox restarts at 1 and produces a second
// "<sandbox>:1" line item — two real intervals that a consumer deduping on that
// id would collapse into one, dropping usage on exactly the path a host failure
// takes. The ledger's own ids stay unique regardless (they carry the host), so
// this protects the public numbering, not the durability spool.
//
// Lowering the floor is refused: sequence numbers only move forward.
func (r *Registry) SetUsageSeqFloor(ctx context.Context, sandboxID string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO usage_seq_floor (sandbox_id, seq) VALUES (?, ?)
		ON CONFLICT(sandbox_id) DO UPDATE SET seq=MAX(seq, excluded.seq)`,
		sandboxID, seq)
	return err
}

// CloseUsageInterval ends billing. cpuUsec is the final cgroup reading, taken
// BEFORE the VM is stopped — the leaf is removed when the VMM exits. A missing
// or unreadable sample passes -1, which keeps whatever the sampler last saw.
//
// Closing an already-closed or absent interval is not an error: every teardown
// path calls this, several can race for one sandbox, and refusing the second
// caller would turn a duplicate close into a failed destroy. That case returns
// ok=false so a caller can tell "nothing was open" from "closed this".
//
// The closed row is returned because it is the only moment its final quantities
// are known in one place: the host's billable counters are credited from it,
// and re-deriving them in the caller would duplicate the MAX() semantics below.
func (r *Registry) CloseUsageInterval(ctx context.Context, sandboxID, reason string, cpuUsec int64) (UsageInterval, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageInterval{}, false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Unix()
	// The MAX against started_at keeps ended_at >= started_at for a sub-second
	// interval, so a fast create/destroy cycle cannot produce a negative
	// duration.
	res, err := tx.ExecContext(ctx,
		`UPDATE usage_intervals
		    SET ended_at=MAX(?, started_at),
		        last_seen_at=MAX(last_seen_at, MAX(?, started_at)),
		        cpu_usec=CASE WHEN ? < 0 THEN cpu_usec ELSE MAX(cpu_usec, MAX(? - cpu_usec_base, 0)) END,
		        end_reason=?
		  WHERE sandbox_id=? AND ended_at IS NULL`,
		now, now, cpuUsec, cpuUsec, reason, sandboxID)
	if err != nil {
		return UsageInterval{}, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return UsageInterval{}, false, err
		}
		return UsageInterval{}, false, tx.Commit()
	}
	// Read back rather than reconstruct. Writes run on one connection and at
	// most one interval per sandbox is ever open, so the highest seq for this
	// sandbox is exactly the row the UPDATE above just closed.
	closed, err := queryUsageTx(ctx, tx, `WHERE sandbox_id=? ORDER BY seq DESC LIMIT 1`, sandboxID)
	if err != nil {
		return UsageInterval{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return UsageInterval{}, false, err
	}
	if len(closed) == 0 {
		return UsageInterval{}, false, nil
	}
	return closed[0], true, nil
}

// CloseAbandonedUsageIntervals closes intervals left open by a server that
// died, at last_seen_at — NOT at now. The host was down; those seconds were not
// served, and billing them is the one error in this ledger a customer would
// certainly notice.
//
// Every open interval is abandoned by definition at startup: intervals only
// exist while a VM does, and VMs only live inside a running server. Returns the
// number closed.
func (r *Registry) CloseAbandonedUsageIntervals(ctx context.Context) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE usage_intervals SET ended_at=last_seen_at, end_reason=? WHERE ended_at IS NULL`,
		EndCrash)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OpenUsageIntervals returns every interval still accruing, for the sampler.
func (r *Registry) OpenUsageIntervals(ctx context.Context) ([]UsageInterval, error) {
	return r.queryUsage(ctx, `WHERE ended_at IS NULL ORDER BY started_at`)
}

// UsageForSandbox returns one sandbox's intervals, oldest first.
func (r *Registry) UsageForSandbox(ctx context.Context, sandboxID string) ([]UsageInterval, error) {
	return r.queryUsage(ctx, `WHERE sandbox_id=? ORDER BY seq`, sandboxID)
}

// UsageBetween returns intervals OVERLAPPING [from, to): an interval that
// started before the window but is still open belongs in the window's bill.
func (r *Registry) UsageBetween(ctx context.Context, from, to time.Time) ([]UsageInterval, error) {
	return r.queryUsage(ctx,
		`WHERE started_at < ? AND (ended_at IS NULL OR ended_at >= ?) ORDER BY started_at`,
		to.Unix(), from.Unix())
}

// UsageQuery selects intervals for a read path. A zero From/To is unbounded and
// an empty SandboxID matches every sandbox, so the zero query is "this host's
// whole ledger".
//
// From/To select by OVERLAP, not containment: an interval that started before
// the window and is still running belongs in that window's usage. The
// consequence is deliberate and must stay visible to callers — a selected
// interval is reported and totalled WHOLE, never clipped to the window. Clipping
// would have to invent a distribution for cpu_usec, which is a single cgroup
// counter for the whole interval and cannot be apportioned over time; a report
// where some quantities were clipped and one was not would be worse than one
// that is uniformly un-clipped and says so.
type UsageQuery struct {
	SandboxID string
	From      time.Time
	To        time.Time
	// Limit bounds the ROWS returned, never the totals. Totals are aggregated
	// in SQL over the full selection, so a truncated page still reports the
	// true amount owed.
	Limit int
}

// UsageTotals is the aggregate of a selection. Billed quantities and consumed
// CPU are kept apart because only the first two are chargeable — see
// docs/usage-metering-plan.md.
type UsageTotals struct {
	Intervals     int64 `json:"intervals"`
	OpenIntervals int64 `json:"open_intervals"`
	// DurationSeconds sums each interval's billable span (to ended_at, or to
	// last_seen_at while open — never to "now").
	DurationSeconds float64 `json:"duration_seconds"`
	VcpuSeconds     float64 `json:"vcpu_seconds"`
	MemMIBSeconds   float64 `json:"mem_mib_seconds"`
	// CPUSeconds is consumed host CPU. Recorded, not billed.
	CPUSeconds float64 `json:"cpu_seconds"`
}

// UsageReport is one answer from the ledger: the rows, plus totals that are
// exact even when the rows are truncated.
type UsageReport struct {
	// HostID is set by a worker answering for its own ledger. A gateway
	// aggregating several hosts leaves it empty and fills Hosts instead.
	HostID string   `json:"host_id,omitempty"`
	Hosts  []string `json:"hosts,omitempty"`
	// SandboxID/From/To echo the query, so a stored response says what it covers.
	SandboxID string          `json:"sandbox_id,omitempty"`
	From      *time.Time      `json:"from,omitempty"`
	To        *time.Time      `json:"to,omitempty"`
	Intervals []UsageInterval `json:"intervals"`
	Totals    UsageTotals     `json:"totals"`
	// Truncated means Limit cut the rows short. Totals still cover everything
	// selected.
	Truncated bool `json:"truncated"`
}

// QueryUsage returns the selected intervals, most recent first, bounded by
// Limit. The bool reports whether rows were cut off.
func (r *Registry) QueryUsage(ctx context.Context, q UsageQuery) ([]UsageInterval, bool, error) {
	where, args := usageWhere(q)
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	// One extra row is the truncation probe: cheaper and race-free compared to
	// a second COUNT that could disagree with the page.
	rows, err := r.queryUsage(ctx, where+` ORDER BY started_at DESC, seq DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// UsageTotalsFor aggregates the same selection in SQL. It is deliberately not
// derived from the returned page: a limited read must still report the true
// amount owed, and money that silently depends on pagination is a bug waiting
// to be believed.
func (r *Registry) UsageTotalsFor(ctx context.Context, q UsageQuery) (UsageTotals, error) {
	where, args := usageWhere(q)
	// The inner select computes each row's billable span once — MAX(...,0)
	// mirrors UsageInterval.Duration's floor at zero — so the outer sums cannot
	// drift from the Go accessors. TestUsageTotalsMatchGoAccessors pins that.
	var t UsageTotals
	err := r.rdb.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(is_open), 0),
		       COALESCE(SUM(dur), 0),
		       COALESCE(SUM(vcpus * dur), 0),
		       COALESCE(SUM(mem_mib * dur), 0),
		       COALESCE(SUM(cpu_usec), 0) / 1e6
		  FROM (SELECT vcpus, mem_mib, cpu_usec,
		               CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END AS is_open,
		               MAX(COALESCE(ended_at, last_seen_at) - started_at, 0) AS dur
		          FROM usage_intervals `+where+`)`, args...).
		Scan(&t.Intervals, &t.OpenIntervals, &t.DurationSeconds, &t.VcpuSeconds, &t.MemMIBSeconds, &t.CPUSeconds)
	return t, err
}

// SumUsage totals intervals in memory. The gateway uses it to combine per-host
// totals it cannot aggregate in SQL; workers use the SQL path.
func SumUsage(intervals []UsageInterval) UsageTotals {
	var t UsageTotals
	for _, iv := range intervals {
		t.Intervals++
		if iv.EndedAt == nil {
			t.OpenIntervals++
		}
		t.DurationSeconds += iv.Duration().Seconds()
		t.VcpuSeconds += iv.VcpuSeconds()
		t.MemMIBSeconds += iv.MemMIBSeconds()
		t.CPUSeconds += iv.CPUSeconds()
	}
	return t
}

// Add combines totals from two selections — used to fold per-host reports into
// one fleet answer.
func (t UsageTotals) Add(o UsageTotals) UsageTotals {
	return UsageTotals{
		Intervals:       t.Intervals + o.Intervals,
		OpenIntervals:   t.OpenIntervals + o.OpenIntervals,
		DurationSeconds: t.DurationSeconds + o.DurationSeconds,
		VcpuSeconds:     t.VcpuSeconds + o.VcpuSeconds,
		MemMIBSeconds:   t.MemMIBSeconds + o.MemMIBSeconds,
		CPUSeconds:      t.CPUSeconds + o.CPUSeconds,
	}
}

func usageWhere(q UsageQuery) (string, []any) {
	clauses := []string{}
	args := []any{}
	if q.SandboxID != "" {
		clauses = append(clauses, `sandbox_id = ?`)
		args = append(args, q.SandboxID)
	}
	if !q.To.IsZero() {
		clauses = append(clauses, `started_at < ?`)
		args = append(args, q.To.Unix())
	}
	if !q.From.IsZero() {
		// Overlap, not containment: an interval still open, or one that ended
		// inside the window, counts however long ago it started.
		clauses = append(clauses, `(ended_at IS NULL OR ended_at >= ?)`)
		args = append(args, q.From.Unix())
	}
	if len(clauses) == 0 {
		return "", args
	}
	return `WHERE ` + strings.Join(clauses, ` AND `), args
}

// UnflushedUsageIntervals returns CLOSED intervals not yet spooled to durable
// storage. Open intervals are excluded: they are not yet facts.
func (r *Registry) UnflushedUsageIntervals(ctx context.Context, limit int) ([]UsageInterval, error) {
	if limit <= 0 {
		limit = 1000
	}
	return r.queryUsage(ctx,
		`WHERE ended_at IS NOT NULL AND flushed_at IS NULL ORDER BY ended_at LIMIT ?`, limit)
}

// MarkUsageFlushed records that these intervals reached durable storage. It
// runs AFTER the write, so a crash in between re-spools them — at-least-once,
// deduped downstream on the deterministic id.
func (r *Registry) MarkUsageFlushed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE usage_intervals SET flushed_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountOpenUsageIntervals is the gauge behind sandbox_usage_intervals_open.
func (r *Registry) CountOpenUsageIntervals(ctx context.Context) (int, error) {
	var n int
	err := r.rdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_intervals WHERE ended_at IS NULL`).Scan(&n)
	return n, err
}

// CountUnflushedUsageIntervals is the gauge behind
// sandbox_usage_unflushed_intervals — the durability backlog to alert on.
func (r *Registry) CountUnflushedUsageIntervals(ctx context.Context) (int, error) {
	var n int
	err := r.rdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_intervals WHERE ended_at IS NOT NULL AND flushed_at IS NULL`).Scan(&n)
	return n, err
}

// PruneUsageIntervals deletes intervals that are closed, already durable, and
// older than cutoff. The bucket is the record of truth; local rows exist only
// to answer recent-usage reads without a bucket round trip.
func (r *Registry) PruneUsageIntervals(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM usage_intervals
		  WHERE ended_at IS NOT NULL AND flushed_at IS NOT NULL AND ended_at < ?`,
		cutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// A sequence floor outlives the intervals it was protecting only for as long
	// as any of them are still here; once a sandbox has no rows left, the floor
	// is protecting nothing and would otherwise accumulate forever.
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM usage_seq_floor
		 WHERE NOT EXISTS (SELECT 1 FROM usage_intervals WHERE usage_intervals.sandbox_id = usage_seq_floor.sandbox_id)`); err != nil {
		return int(n), err
	}
	return int(n), nil
}

const usageCols = `id, sandbox_id, seq, host_id, vm_id, vcpus, mem_mib, metadata, started_at, ended_at, last_seen_at, cpu_usec, cpu_usec_base, end_reason, flushed_at`

// usageQuerier is satisfied by both *sql.DB and *sql.Tx, so a read inside the
// close transaction sees its own uncommitted UPDATE.
type usageQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (r *Registry) queryUsage(ctx context.Context, where string, args ...any) ([]UsageInterval, error) {
	return queryUsageTx(ctx, r.rdb, where, args...)
}

func queryUsageTx(ctx context.Context, q usageQuerier, where string, args ...any) ([]UsageInterval, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+usageCols+` FROM usage_intervals `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UsageInterval{}
	for rows.Next() {
		var (
			u                 UsageInterval
			meta              string
			started, lastSeen int64
			ended, flushed    sql.NullInt64
		)
		if err := rows.Scan(&u.ID, &u.SandboxID, &u.Seq, &u.HostID, &u.VMID, &u.Vcpus, &u.MemMIB,
			&meta, &started, &ended, &lastSeen, &u.CPUUsec, &u.CPUUsecBase, &u.EndReason, &flushed); err != nil {
			return nil, err
		}
		u.StartedAt = time.Unix(started, 0).UTC()
		u.LastSeenAt = time.Unix(lastSeen, 0).UTC()
		if ended.Valid {
			t := time.Unix(ended.Int64, 0).UTC()
			u.EndedAt = &t
		}
		if flushed.Valid {
			t := time.Unix(flushed.Int64, 0).UTC()
			u.FlushedAt = &t
		}
		if meta != "" && meta != "{}" {
			_ = json.Unmarshal([]byte(meta), &u.Metadata)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func usageIntervalID(hostID, sandboxID string, seq int64) string {
	if hostID == "" {
		hostID = "unknown"
	}
	return fmt.Sprintf("%s:%s:%d", hostID, sandboxID, seq)
}

func nonNilMetadata(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
