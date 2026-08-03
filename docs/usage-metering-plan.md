# Usage metering plan

Status: **Phase 1 shipped** (ledger, sampler, lifecycle hooks, crash recovery).
Phases 2–4 are design only.

Goal: per-sandbox CPU-hours and RAM-hours, durable enough to bill a customer
from. Disk is deliberately **not** metered — it is not elastic today, so it is
free.

## Decisions

1. **Record both CPU bases, price on allocated.** Every interval stores
   allocated `vcpu_seconds` (`vcpus × duration`) *and* consumed `cpu_seconds`
   (from the VM's cgroup). Invoices use allocated: it is predictable and matches
   e2b/Modal. Consumed exists because CPU is deliberately oversubscribed ~6:1
   and margin analysis, abuse detection, and any future burst SKU all need it —
   and a metric that was never recorded cannot be backfilled.
2. **RAM is allocated-only.** `memory.current` is readable but with no
   virtio-balloon a guest's dirtied pages never return, so actual RSS converges
   on allocated anyway (see "No memory overcommit" in `CLAUDE.md`). It is also
   not something a customer can control, so it is not a fair billing base.
3. **Closed intervals become durable in GCS**, spooled as newline-JSON by each
   host. The host SQLite is the working store; the bucket is the record.
4. **Owner attribution is deferred.** The ledger keys on `sandbox_id` and has no
   owner column. There is no tenant model yet (P0.7 in
   [production-readiness-plan.md](production-readiness-plan.md)), and inventing
   a client-asserted one would produce a number that looks authoritative and
   isn't. See [Attribution](#attribution-deferred-but-not-lost) for the cheap
   hedge that keeps this recoverable.

## The unit: one interval per VMM lifetime

A billable interval is exactly the span one Firecracker process serves a
user-visible sandbox. This is not an imposed abstraction — it is what the
existing lifecycle already produces:

- Every jailed VM gets a cgroup-v2 leaf at `<cgroup_root>/<parent>/<vmid>`
  (`jailerCgroupLeaf`, `internal/vm/jailer.go:893`) holding `cpu.stat`.
- The leaf is created per launch and removed by `launchCleanup` when the machine
  exits (`internal/vm/machine_linux.go:108`).
- Every transition that ends compute — destroy, hibernate, TTL reap, unexpected
  VM exit, `shutdownAll` — also ends that leaf.

So `cpu.stat`'s `usage_usec`, read at close, **is** the interval's consumed CPU.
No cross-interval subtraction, no counter-reset handling, no risk of attributing
one sandbox's CPU to another. A hibernate/wake cycle produces two intervals
because it produces two VMMs, which is also exactly what we want to bill.

### What does and does not accrue

| Status | Bills | Why |
| --- | --- | --- |
| `running` | yes | the sandbox is serving the customer |
| `starting` | no | our bring-up latency, charged to us |
| `stopping` | no | teardown |
| `preparing`, `warming` | **never** | ready-pool VMs are platform overhead. At `warm_pool_size: 8` per worker, billing these would invent phantom usage on every host |
| `hibernated` | no CPU, no RAM | the VM is gone and its memory is released; disk is free by decision |

Interval **open** is therefore on `MarkRunning` and on warm-claim promotion
(`ClaimWarm`), never on `POST /sandboxes` acceptance. A create that fails, or
that the gateway fails over to another host, never reaches `MarkRunning` and so
can never bill — the failover double-billing question resolves itself.

Note the consequence of the hibernation row: a customer can park sandboxes
indefinitely for free. That is the intended product story (park a devbox at zero
cost), but the only thing bounding it is the host **port** pool, since
hibernation releases tap/IP and holds only the port. If that becomes a problem
the answer is a parked-sandbox count quota, not a disk charge.

## Schema

New table in the host registry. It must be a separate table because
`registry.Destroy` deletes the sandbox row outright
(`internal/registry/registry.go:1194`) — usage of a terminated sandbox has
nowhere to live today.

```sql
CREATE TABLE IF NOT EXISTS usage_intervals (
  id            TEXT PRIMARY KEY,   -- host_id:sandbox_id:seq, stable across replays
  sandbox_id    TEXT NOT NULL,
  seq           INTEGER NOT NULL,   -- 1-based per sandbox; hibernate/wake increments
  host_id       TEXT NOT NULL,
  vm_id         TEXT NOT NULL,
  vcpus         INTEGER NOT NULL,   -- effective, snapshotted at open
  mem_mib       INTEGER NOT NULL,   -- effective, snapshotted at open
  metadata      TEXT NOT NULL DEFAULT '{}',
  started_at    INTEGER NOT NULL,
  ended_at      INTEGER,            -- NULL = open
  last_seen_at  INTEGER NOT NULL,   -- sampler heartbeat; crash-truncation point
  cpu_usec      INTEGER NOT NULL DEFAULT 0,
  end_reason    TEXT NOT NULL DEFAULT '',
  flushed_at    INTEGER             -- NULL = not yet spooled to the bucket
);
CREATE INDEX IF NOT EXISTS idx_usage_open    ON usage_intervals(ended_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_usage_flush   ON usage_intervals(flushed_at) WHERE flushed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_usage_sandbox ON usage_intervals(sandbox_id, seq);
```

`vcpus`/`mem_mib` are snapshotted rather than joined, because the sandbox row is
gone by invoice time and because `0` means "template default" in the registry —
`effectiveResources` (`internal/server`) resolves it, and the resolved value is
what we bill.

Everything billable is derived at read time, so no rounding is baked into
storage:

```
duration_s      = (ended_at ?? last_seen_at) - started_at
vcpu_seconds    = vcpus   * duration_s      -- billed
mem_mib_seconds = mem_mib * duration_s      -- billed
cpu_seconds     = cpu_usec / 1e6            -- recorded, not billed
```

## Sampling

New linux/stub pair in `internal/vm` — matching the existing build-tag
convention (`//go:build linux` / `!linux`, identical signatures):

```go
type UsageSample struct { CPUUsec uint64; MemCurrentBytes uint64 }
func SampleUsage(m *Machine) (UsageSample, error)
```

- Jailed path: read `cpu.stat` from the machine's leaf. The leaf path derives
  from the machine's `vmid` plus the jailer config, so `Machine` needs to retain
  the resolved leaf (it already retains `launchCleanup` closed over it).
- Unjailed dev path (`VMIsolation != "jailer"`, macOS stub): fall back to
  `utime + stime` from `/proc/<pid>/stat` via the existing `vm.PID`. This
  undercounts kernel-side work outside the process; acceptable for dev, and the
  fallback is recorded in `end_reason` so a mixed dataset is never mistaken for
  clean data.

Two writers, both cheap:

- **Close hooks**, each already holding `wakeLock(id)`, so no new locking:
  `destroyLocked` (`internal/server/server.go:1263`), the hibernate freeze path,
  `destroyExpired` (`internal/server/ttl.go:90`), `cleanupExitedMachine`
  (`internal/server/machine_watch.go:32`), and `shutdownAll`. Each samples
  **before** stopping the VM, because `launchCleanup` removes the leaf on exit.
  The few hundred milliseconds of teardown CPU this misses is not worth a hook
  inside `vm`'s cleanup path.
- **A 60 s sampler** advancing `last_seen_at` and `cpu_usec` on every open
  interval. This exists for crash containment: a host that dies loses at most
  one sample window of a bill rather than the whole interval.

## Crash recovery

`reconcile()` at startup closes every interval left open by a dead serve, at
`last_seen_at` and with `end_reason = "crash"` — **never at `now`**. The host
was down; those seconds were not served, and billing them is the one error in
this system a customer would actually notice.

This is consistent with how reconcile already treats sandbox rows ("every row is
stale by definition at startup"), with the same hibernated-row exception: a
hibernated sandbox has no open interval to close.

## Durability

A worker's SQLite lives on its data disk, which a MIG scale-in that *deletes* an
instance takes with it. So closed intervals are spooled to the durability
bucket, following the commit-marker pattern already used for hibernation records
(`internal/server/hib_durable.go`) via the existing `internal/gcsblob` client:

```
usage/<host_id>/<YYYY-MM-DD>/<flush_seq>.jsonl
```

- Immutable, append-only objects; one flush = one object, written whole. No
  read-modify-write, so no CAS contention between hosts (each host owns its own
  prefix).
- Flush loop: every ~5 min and on graceful shutdown, take rows with
  `flushed_at IS NULL AND ended_at IS NOT NULL`, write one object, then stamp
  `flushed_at`. At-least-once: a crash between write and stamp re-emits, and the
  aggregator dedups on `id` (`host_id:sandbox_id:seq`), which is why that key is
  deterministic rather than a UUID.
- Rows are retained locally for a bounded window after flush (say 7 days) so the
  live API can answer recent-usage queries without touching the bucket, then
  pruned.
- Open intervals are **not** spooled — they are not yet facts. Their loss
  ceiling is the 60 s sampler plus the crash-truncation rule.

This is the step not to defer. An immutable spool means the aggregator, the
schema of the warehouse, even the pricing model can all be rebuilt later; a lost
write cannot.

## Read surfaces

**Prometheus gets host-level counters only** — no `sandbox_id` label. At
hundreds of churning sandboxes per host that label is a cardinality bomb, and
Prometheus is the wrong store for money anyway. Added to
`internal/server/metrics.go` alongside the existing counters:

```
sandbox_billable_vcpu_seconds_total
sandbox_billable_mem_mib_seconds_total
sandbox_cpu_seconds_total
sandbox_usage_intervals_open
sandbox_usage_unflushed_intervals      # alert on this: durability falling behind
```

These federate through the gateway's `/metrics/hosts` with a `host` label for
free (`injectHostLabel`, `internal/gateway/federate.go`).

**Per-sandbox detail is API-only:**

- `GET /v1/sandboxes/{id}/usage` — intervals plus totals for one sandbox.
- `GET /v1/usage?from=&to=` — this host's ledger, paginated.
- On the gateway both scatter-gather across live hosts exactly like
  `handleList` (`internal/gateway/gateway.go:1258`). The gateway stays
  **stateless** — it holds no durable billing state, consistent with the design
  note that a shared DB would break each host's reconcile invariants.

The scatter-gather caveat must be documented in the response, not hidden: the
live API can only report what live hosts still hold. Usage from a sandbox on a
since-deleted worker exists only in the bucket. The API is for dashboards and
debugging; **the bucket is the billing source of truth.**

## Attribution (deferred, but not lost)

No owner column, per decision 4. But the ledger snapshots the sandbox's
`metadata` JSON at interval open (already a first-class create/PATCH field,
≤64 keys, `internal/server/server.go:1176`). This is not an owner concept and
must not be presented as one — it is a hedge that costs one column and means
that whatever labels callers happen to set today (`team`, `project`, whatever)
are recoverable when attribution is designed properly. Snapshotting at open also
means a later `PATCH` of metadata cannot rewrite billing history.

When tenant auth lands, add `owner_id` populated from the auth context, and
backfill from these snapshots where they happen to carry something usable.

## Phases

**Phase 1 — ledger and truth. DONE.** `internal/registry/usage.go` (migration +
interval CRUD), `internal/vm/usage*.go` (`SampleUsage`, linux/stub pair, parsing
split out so the `/proc` and cgroup formats are testable off-Linux),
`internal/server/usage.go` (the meter), hooks at all seven lifecycle sites, the
60 s sampler, and reconcile crash-truncation.

Two things the implementation added beyond this design:

- **The sampler closes an interval whose VM is gone** rather than extending it.
  A missed close would otherwise grow a customer's invoice forever — the only
  failure mode here that silently costs real money — so "no VMM means not
  accruing" is enforced on every tick, not just trusted at the close sites.
- **Ledger writes are detached from their caller's context**
  (`meterCtx` = `context.WithoutCancel` + 5 s). Both hooks sit on request paths:
  a client disconnecting mid-destroy would have cancelled the close, and a
  create whose caller went away just after `ClaimWarm` would have handed out a
  running sandbox that bills nothing.

`end_reason` distinguishes `destroy` / `hibernate` / `expire` / `shutdown` /
`vm_exit` / `crash`. `expire` and `shutdown` are attributed deliberately: a
TTL reap is the answer to "why did my sandbox vanish", and a fleet roll must not
read as every customer's sandbox going idle at the same instant.

**Phase 2 — durability.** GCS spool, flush loop, retention prune,
`sandbox_usage_unflushed_intervals` + an alert.

**Phase 3 — read paths.** `GET /v1/usage`, `GET /v1/sandboxes/{id}/usage`,
gateway scatter-gather, host counters, `api/openapi.yaml` + SDK method
(remember `npm run check:api` fails on drift).

**Phase 4 — later, out of scope here.** Invoice rollup over the spool, tenant
auth binding, quotas.

## Tests

The Go side is thin by design (`CLAUDE.md`), and this is money, so it earns
real unit coverage:

- `internal/registry`: interval algebra — open/close, `seq` increment across a
  hibernate/wake cycle, no two open intervals for one sandbox, crash-truncation
  closing at `last_seen_at`.
- `internal/server`: warm-pool VMs never open an interval before `ClaimWarm`;
  a failed create leaves no interval; a wake after hibernate opens `seq+1`;
  `effectiveResources` (not raw `0`) is what gets snapshotted.
- Spool: at-least-once replay dedups on `id`; a flush that fails leaves
  `flushed_at` NULL and retries.
- Fleet e2e (`tests/`): create → exec → hibernate → wake → destroy, then assert
  the ledger reports two intervals with plausible non-zero CPU and the expected
  RAM-seconds.

## Open questions

- **Rounding and minimums.** Per-second billing with no minimum is the honest
  default and the easiest to explain, but a 12 ms create (measured p50 for a
  ready-pool hit) means a sandbox can cost effectively nothing. Whether that
  needs a per-create minimum is a pricing decision, not a metering one — the
  ledger stores raw microseconds either way.
- **Retention of the spool.** Bucket lifecycle rules should keep usage objects
  far longer than snapshot artifacts; they are the audit trail for invoices.
- **Snapshot storage.** Deliberately unmetered here along with disk, but user
  snapshots *are* elastic (they accumulate in GCS), so they will need metering
  before disk does.
