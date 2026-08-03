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
	// Metadata is the sandbox's labels as of interval open. Not an owner
	// concept (there is no tenant model yet) — it is recorded so that whatever
	// callers label sandboxes with today stays recoverable when attribution is
	// designed, and so a later PATCH cannot rewrite billing history.
	Metadata  map[string]string `json:"metadata,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	// EndedAt is nil while the interval is open.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// LastSeenAt is advanced by the sampler. It is the truncation point for an
	// interval left open by a crashed server: those seconds after it were not
	// served, so they are not billed.
	LastSeenAt time.Time `json:"last_seen_at"`
	// CPUUsec is host CPU time consumed by the VMM, from the cgroup leaf.
	// Recorded but not billed — see docs/usage-metering-plan.md.
	CPUUsec   int64      `json:"cpu_usec"`
	EndReason string     `json:"end_reason,omitempty"`
	FlushedAt *time.Time `json:"-"`
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
	`)
	return err
}

// OpenUsageInterval starts billing a sandbox. Callers pass EFFECTIVE resources
// (template defaults already resolved) because the registry stores 0 to mean
// "default" and only the server knows the template.
//
// It is called when a sandbox becomes usable — MarkRunning, a warm claim, or a
// wake — never at create acceptance: bring-up latency and pre-claim ready-pool
// runtime are the platform's cost, not the customer's.
//
// Opening twice for one sandbox returns ErrUsageOpen instead of double-billing.
func (r *Registry) OpenUsageInterval(ctx context.Context, sandboxID, hostID, vmID string, vcpus, memMIB int64, metadata map[string]string) (UsageInterval, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return UsageInterval{}, err
	}
	defer tx.Rollback()

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM usage_intervals WHERE sandbox_id=?`, sandboxID).Scan(&maxSeq); err != nil {
		return UsageInterval{}, fmt.Errorf("read usage seq: %w", err)
	}

	meta, err := json.Marshal(nonNilMetadata(metadata))
	if err != nil {
		return UsageInterval{}, fmt.Errorf("encode usage metadata: %w", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	u := UsageInterval{
		ID:         usageIntervalID(hostID, sandboxID, maxSeq.Int64+1),
		SandboxID:  sandboxID,
		Seq:        maxSeq.Int64 + 1,
		HostID:     hostID,
		VMID:       vmID,
		Vcpus:      vcpus,
		MemMIB:     memMIB,
		Metadata:   metadata,
		StartedAt:  now,
		LastSeenAt: now,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO usage_intervals (id, sandbox_id, seq, host_id, vm_id, vcpus, mem_mib, metadata, started_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.SandboxID, u.Seq, u.HostID, u.VMID, u.Vcpus, u.MemMIB, string(meta),
		u.StartedAt.Unix(), u.LastSeenAt.Unix()); err != nil {
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
		`UPDATE usage_intervals SET last_seen_at=?, cpu_usec=MAX(cpu_usec, ?)
		 WHERE sandbox_id=? AND ended_at IS NULL`,
		time.Now().UTC().Unix(), cpuUsec, sandboxID)
	return err
}

// CloseUsageInterval ends billing. cpuUsec is the final cgroup reading, taken
// BEFORE the VM is stopped — the leaf is removed when the VMM exits. A missing
// or unreadable sample passes -1, which keeps whatever the sampler last saw.
//
// Closing an already-closed or absent interval is not an error: every teardown
// path calls this, several can race for one sandbox, and refusing the second
// caller would turn a duplicate close into a failed destroy.
func (r *Registry) CloseUsageInterval(ctx context.Context, sandboxID, reason string, cpuUsec int64) error {
	now := time.Now().UTC().Unix()
	// COALESCE keeps ended_at >= started_at for a sub-second interval, so a
	// fast create/destroy cycle cannot produce a negative duration.
	_, err := r.db.ExecContext(ctx,
		`UPDATE usage_intervals
		    SET ended_at=MAX(?, started_at),
		        last_seen_at=MAX(last_seen_at, MAX(?, started_at)),
		        cpu_usec=CASE WHEN ? < 0 THEN cpu_usec ELSE MAX(cpu_usec, ?) END,
		        end_reason=?
		  WHERE sandbox_id=? AND ended_at IS NULL`,
		now, now, cpuUsec, cpuUsec, reason, sandboxID)
	return err
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
	return int(n), nil
}

const usageCols = `id, sandbox_id, seq, host_id, vm_id, vcpus, mem_mib, metadata, started_at, ended_at, last_seen_at, cpu_usec, end_reason, flushed_at`

func (r *Registry) queryUsage(ctx context.Context, where string, args ...any) ([]UsageInterval, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT `+usageCols+` FROM usage_intervals `+where, args...)
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
			&meta, &started, &ended, &lastSeen, &u.CPUUsec, &u.EndReason, &flushed); err != nil {
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
