# Sandbox stats: surfacing live resource utilization

Status: **phases 1–3 implemented, not yet deployed.** Phase 4 remains a
plan. Companion to `docs/usage-metering-plan.md`, which covers the *billing*
half of the same question and is already shipped.

What landed:

- **Phase 1** (host-side): `internal/server/sandboxmetrics.go` — sampler, ring,
  `GET /sandboxes/{id}/metrics`, `GET /v1/sandboxes/{id}/metrics`, and the
  aggregate `/metrics` series. Config: `metrics_interval_sec`,
  `metrics_history`. Ships with an ordinary `rollout.sh`.
- **Phase 2** (guest-side): `cmd/sandboxd/stats.go` — `GET /stats`, merged into
  the sample behind `metrics_guest_stats` (default **off**). The agent is
  image-pinned, so this needs `bake-image.sh bake && golden` plus a MIG roll
  before the flag can be turned on.
- **Phase 3** (clients): `api/openapi.yaml` + regenerated `api-v1.ts`, SDK
  `sandbox.metrics()`, CLI `sandbox metrics <id>`, `docs/api-v1.md`. The SDK
  version is deliberately **not** bumped — releasing is its own five-step ritual.

The wire types live in `internal/metricsapi` so the worker, the CLI client and
the v1 adapter share one definition.

## What this is not

The usage ledger (`internal/registry/usage.go`, `GET /v1/usage`) already answers
"what is this sandbox costing" — allocated vCPU-seconds and MiB-seconds per VMM
lifetime, spooled durably to GCS. It deliberately bills **allocation**, and
records consumed `cpu_usec` without billing it.

What no API answers today is **utilization**: is this sandbox's CPU pegged, how
much of its RAM is actually in use, how much disk has it eaten, how much
traffic has it moved. That's the gap, and it has two distinct audiences that
want different numbers:

- **Tenant / SDK caller** — "is my agent's build stuck, did it OOM, is the disk
  full". Wants guest-perceived numbers over a short window.
- **Control plane / operator** — "is 6:1 CPU oversubscription actually safe, is
  `MEM_PER_SLOT_MIB` right, which sandboxes are idle enough to freeze". Wants
  host-truth aggregates, no per-sandbox cardinality.

They are served by one collector and two very different exposition paths. Not
noticing that is how this feature turns into an accidental TSDB.

## Prior art

### E2B — the only one with a real utilization API

| Surface | Shape |
| --- | --- |
| SDK | `await sbx.getMetrics()`, `Sandbox.getMetrics(id)` / `sbx.get_metrics()` |
| REST | `GET /sandboxes/{sandboxID}/metrics?start=&end=` |
| CLI | `e2b sandbox metrics <sandbox_id>` |

Returns an **array of timestamped samples**, collected **every 5 s**:

```
timestamp     ISO 8601
cpuUsedPct    percent
cpuCount      cores
memUsed       bytes
memTotal      bytes
diskUsed      bytes
diskTotal     bytes
```

Notable details:

- Empty array until the first sample lands ("it may take a second or more") —
  they made the cold window an API-visible state rather than blocking.
- Collected by **`envd`**, the in-VM daemon (our `sandboxd` equivalent), shipped
  over OpenTelemetry into **ClickHouse**. So the series is a real time-series
  store, not a ring buffer, and it is *guest-sourced* — `memUsed`/`memTotal` and
  `diskUsed`/`diskTotal` are what the guest sees, not what the hypervisor pays.
- No network counters, no disk I/O, no per-process breakdown, and nothing
  host-side (VMM RSS, CoW rootfs growth) is exposed to tenants.

### Tensorlake — nothing

Their sandbox docs (`docs.tensorlake.ai/sandboxes/*`) publish no metrics,
monitoring or observability page at all. `SandboxInfo` carries
`resources.cpus` / `resources.memory_mb` — **allocation, not utilization** —
plus `status` and lifecycle timestamps. The only runtime visibility is
process logs (stdout/stderr). Billing is per-second-alive with CPU + RAM + disk
as separate line items, i.e. the same allocation-based model our ledger already
implements.

### What that means for us

E2B's array-of-samples shape is the de-facto contract, and it's small enough to
match field-for-field so an SDK adapter is trivial. But it is also the *floor*:
they surface four resources, all guest-side. Our microVM gives us signals E2B
doesn't publish and that our own fleet needs — tap byte counters, cgroup CPU
truth, and per-sandbox CoW rootfs growth on a shared XFS data disk. Match their
names where the semantics match; add ours alongside rather than pretending
guest numbers answer host questions.

## What we can measure, and from where

### Host-side — free, no guest contact, works today

| Signal | Source | Notes |
| --- | --- | --- |
| CPU consumed | jailer cgroup v2 leaf `cpu.stat` → `usage_usec` | **Already sampled** every 60 s by `usageSampler` for billing (`internal/vm/usage_linux.go`). Delta ÷ (elapsed × vcpus) → utilization percent. |
| Host memory | same leaf, `memory.current` | **Already read** into `UsageSample.MemBytes` and then discarded. |
| Network | `/sys/class/net/<tap>/statistics/{rx,tx}_{bytes,packets}` | Tap name is on the registry row (`Sandbox.TapDevice`). **Polarity inverts**: the tap's `rx` is the guest's `tx`. |
| Disk I/O | same leaf, `io.stat` (`rbytes`/`wbytes`) | Only under the jailer, and only for devices the leaf accounts. |
| Rootfs growth | `stat` the per-VM rootfs → `st_blocks × 512` | Real incremental bytes under reflink CoW. E2B has no equivalent. |

`memory.current` deserves a warning, because it is the number most likely to be
mislabeled "memory used" and shipped: it is the VMM's charge, i.e. guest pages
**touched**, and without a virtio-balloon (see CLAUDE.md, "No memory overcommit")
freed guest pages never come back until hibernation. It is a high-water mark of
what the host is paying, which is exactly the operator's question and exactly
*not* the tenant's. Surface it as `host_mem_bytes`, never as `mem_used_bytes`.

### Guest-side — needs a new `sandboxd` endpoint

| Signal | Source |
| --- | --- |
| `mem_total_bytes` / `mem_used_bytes` | `/proc/meminfo`: `MemTotal`, `MemTotal - MemAvailable` (E2B semantics) |
| `disk_total_bytes` / `disk_used_bytes` | `statfs("/")` |
| `load1` | `/proc/loadavg` |
| `processes` | `/proc/loadavg` field 4, or a `/proc` dirent count |

So: **CPU, network and rootfs bytes are free host-side; only memory-in-use and
disk-free genuinely require the guest.** That split is what the phasing below is
built on, because a `sandboxd` change is image-pinned (rebake + MIG roll) while a
host change is one `rollout.sh`.

`/proc` is mounted in template guests too — `runInit` mounts it before re-exec
(`cmd/sandboxd/init_linux.go`) — so container-image templates are covered.

## Four constraints that will bite

1. **Sampling must not count as activity.** Every host→guest path today runs
   through the activity tracker (`act.begin`/`touch` in
   `internal/server/hibernate.go`), which resets the idle clock and pins the
   sandbox. A 5 s poll into every guest would **disable idle hibernation
   fleet-wide** — silently converting a hibernate-heavy fleet into a fully
   resident one, blowing `mem_budget_mib` and every customer's bill. The guest
   sampler needs a path that never touches the tracker. This is the single
   biggest footgun in the feature and gets a dedicated regression test.
2. **Never wake a hibernated sandbox to sample it.** The sampler iterates
   `s.machines` (live VMs only) and never calls `ensureRunning`. A read for a
   frozen sandbox returns its last known samples plus `state: "hibernated"` —
   not zeros, not a wake.
3. **Per-sandbox series never go to Prometheus.** `internal/server/metrics.go`
   already documents this decision for billing ("a `sandbox_id` label at this
   churn rate is a cardinality bomb"). The same applies here, harder — 5 s
   samples × 8 gauges × fleet churn. Per-sandbox detail is served from a bounded
   in-memory ring on the owning worker; Prometheus gets host **aggregates** only.
   And it does not go in SQLite either: 128 VMs at 5 s is 25 rows/s of pure
   churn against the same database the create path commits into.
4. **The gateway holds no durable state.** Metrics are read id-routed to the
   owning host. A sandbox that changed hosts (pause/resume, B4 adopt) starts a
   fresh series — the same honest caveat `/v1/usage` already prints as
   `coverage.scope: live_hosts`.

## Design

### Sample

One merged struct, host fields always present, guest fields present only when
the agent answered. Names track E2B where the semantics match.

```
timestamp             RFC3339
sequence              int    -- VMM lifetime, shared with the usage ledger
cpu_count             int    -- allocated vcpus (effectiveResources)
cpu_used_pct          float  -- cgroup delta / (elapsed × vcpus) × 100
cpu_seconds_total     float  -- raw counter, monotone WITHIN a sequence
host_mem_bytes        int    -- cgroup memory.current (what the host pays)
mem_total_bytes       int    -- guest
mem_used_bytes        int    -- guest: MemTotal - MemAvailable
disk_total_bytes      int    -- guest statfs /
disk_used_bytes       int    -- guest statfs /
rootfs_alloc_bytes    int    -- host: st_blocks × 512
net_rx_bytes          int    -- guest perspective (tap tx)
net_tx_bytes          int    -- guest perspective (tap rx)
load1                 float  -- guest
processes             int    -- guest
```

`sequence` is the payoff of reusing the ledger's numbering: every counter in
this sample resets when the VMM is replaced (restore, wake, fanout), and a
consumer detecting a reset by "the number went down" is a classic bug. Publish
the lifetime number instead and the reset is self-describing — and it joins
directly against `GET /v1/sandboxes/{id}/usage`.

### Collection

One goroutine per worker, one ticker (`metrics_interval_sec`, default 5, `0` =
off), started next to `usageSampler`:

- iterate `s.machines` — in-memory, no SQLite scan, running VMs only;
- host part: 2–4 small file reads per VM. 128 VMs ≈ 500 reads / 5 s. Free;
- guest part (phase 2, `metrics_guest_stats`): bounded concurrency (~8), 1 s
  timeout, **activity-tracker-free**, and — critically — over a transport keyed
  on the **sandbox id**, not the guest IP. The IP-recycling hazard documented in
  `internal/server/proxy.go` (`agentAuthority`) applies here exactly as it does
  to exec; a metrics poller with its own naively-keyed pool would reintroduce the
  `connection reset by peer` class of bug on a schedule;
- guest part runs on a slower multiple of the tick (default every 2nd, i.e.
  10 s) — the poll is itself observable in the tenant's own `cpu_used_pct`, and
  halving that observer effect costs nothing;
- an agent that 404s `/stats` (old baked sandboxd) degrades to host-only fields.
  Logged once per VM, never fatal — same pattern as `/clock`.

Ring buffer per sandbox: fixed cap `metrics_history` samples (default 360 = 30 min
at 5 s). At ~120 B/sample that's ~43 KB per sandbox, ~5.5 MB for 128 — bounded and
predictable. Dropped on destroy; **retained across hibernation** so a wake
continues the same series with a visible gap and a new `sequence`.

### API

Worker (legacy route, where every real handler lives):

```
GET /sandboxes/{id}/metrics?from=&to=&limit=
→ {"samples":[…], "state":"running|hibernated", "interval_seconds":5}
```

Public v1 (`internal/apiv1`, ~40 lines — `h.call` replays into the same mux, so
on the gateway it lands on `handleProxyByID` and on a worker it hits the handler
directly; the generic `/sandboxes/{id}/{rest...}` proxy route means **no gateway
change is needed**):

```
GET /v1/sandboxes/{id}/metrics?from=&to=&limit=
```

`?limit=1` is the "latest sample" call — no second endpoint. Empty array before
the first sample, exactly as E2B does; don't block a read waiting for a tick.

Deliberately **not** built: a fleet-wide `/v1/metrics` scatter-gather of
per-sandbox series. That is a TSDB product, and we have Prometheus.

Deliberately **not** added: a `metrics` sub-object on `GET /v1/sandboxes/{id}`.
It would make the hottest read in the API do work, and E2B keeps them separate
for the same reason.

### Prometheus — the control-plane half

From the same sampler, host-level aggregates with **no sandbox label**, which
federate through the gateway's `/metrics/hosts` with a `host` label for free:

```
sandbox_guest_cpu_seconds_total          counter  live, not credit-at-close
sandbox_guest_mem_used_bytes             gauge    sum over running sandboxes
sandbox_guest_host_mem_bytes             gauge    sum of cgroup memory.current
sandbox_rootfs_alloc_bytes               gauge    sum, CoW growth on the data disk
sandbox_net_bytes_total{dir="rx|tx"}     counter
sandbox_cpu_utilization_bucket{le=…}     histogram of per-sandbox cpu_used_pct
```

That histogram is the highest-value series in the whole plan and costs one extra
line: **nothing today measures whether the deliberate ~6:1 CPU oversubscription
is safe**, or how many resident sandboxes are doing literally nothing. Same for
`sandbox_rootfs_alloc_bytes` — a guest running `dd` today eats the shared XFS
data disk with no signal until a create fails.

## Phasing

**Phase 1 — host-side only** (~1 day; ships with a plain `rollout.sh`, no rebake)
sampler + ring + `GET /sandboxes/{id}/metrics` + `GET /v1/sandboxes/{id}/metrics`
+ the aggregate Prometheus series + a Grafana row. Fields: `cpu_*`,
`host_mem_bytes`, `net_*`, `rootfs_alloc_bytes`, `sequence`. Config:
`metrics_interval_sec`, `metrics_history`.

**Phase 2 — guest-side** (~half day of code, plus a rebake + MIG roll)
`GET /stats` in `sandboxd` (one handler, three `/proc` reads and a `statfs`),
`agentapi.Stats`, merged into the sample. Adds `mem_used/mem_total`, `disk_*`,
`load1`, `processes` → **full E2B field parity**. Gated by
`metrics_guest_stats`. Remember: the agent is image-pinned —
`bake-image.sh bake && golden` then roll the MIG; `rollout.sh` alone does not
ship it.

**Phase 3 — clients** (~half day)
`api/openapi.yaml` + regenerated `src/generated/api-v1.ts`, SDK
`sandbox.metrics({from, to, limit})`, CLI `sandbox metrics <id>` (table +
crude sparkline), `docs/api-v1.md`. Follow the five-step SDK release ritual in
CLAUDE.md — a bumped `package.json` is not a release.

**Phase 4 — optional, only on a real request**
(a) retention beyond the ring: downsample to 1-min averages and spool alongside
usage into `gs://…/metrics/<host>/<date>/`, reusing the usage spooler; or ship
per-sandbox series to the existing Prometheus behind a flag for bounded fleets;
or do what E2B did and put them in ClickHouse. Pick one deliberately — the ring
is *not* a stepping stone to all three.
(b) CPU-aware hibernation: today "idle" means no API and no forwarded-port
traffic, so a sandbox mid-build with no API chatter is freeze-eligible. With
utilization sampled, the reaper can skip anything above a CPU threshold. This is
a behavior change to a load-bearing subsystem — separate change, separate test
campaign, not bundled into a read-only feature.

## Testing

- Unit: delta-over-elapsed math, counter reset across a `sequence` change, ring
  eviction, tap counter polarity inversion. `sampleCgroup` already takes the
  leaf as a parameter, so a temp dir with fake `cpu.stat`/`memory.current`
  covers the host reader without root.
- Unit: an unreadable leaf / vanished tap yields a sample with those fields
  absent, never a failed tick and never a zero that reads as "idle".
- **Regression, mandatory**: a sandbox with metrics enabled and no client
  traffic still hibernates within its window. This is constraint 1; if it ever
  fails, the fleet's memory budget is wrong and the bills are too high.
- Fleet e2e in `tests/`: create → `exec yes > /dev/null` → assert
  `cpu_used_pct` crosses a threshold and falls back after the kill; write a
  200 MB file → assert `disk_used_bytes` and `rootfs_alloc_bytes` both move;
  hibernate → assert the read returns `state: hibernated` with the pre-freeze
  samples intact and **no wake occurred**; wake → assert a new `sequence`.
- Overhead check: sampler on/off against a 64-way burst, comparing create p50/p95
  from `docs/benchmarks.md`. Drive it from the control VM, not a laptop.

## Open questions

1. **Tenant-facing or operator-facing first?** If the near-term need is fleet
   density and idle detection, phase 1's Prometheus aggregates are ~80% of the
   value and phase 3 can wait indefinitely. If it's SDK parity with E2B, phase 2
   is mandatory and should be scheduled with the next rebake.
2. **Retention.** Is 30 min in RAM (lost on a worker restart, gone when a
   sandbox moves hosts) acceptable, or does someone need to query yesterday? The
   answer decides whether phase 4a exists, and it is much cheaper to decide now
   than to bolt a store on later.
3. **Interval.** 5 s matches E2B and costs ~500 file reads per tick per host. If
   nobody needs sub-minute resolution, 15 s makes the guest poll's observer
   effect vanish and the ring cover 90 min at the same memory.
