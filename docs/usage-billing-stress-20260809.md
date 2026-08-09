# Billing stress campaign — 2026-08-09

What the billable ledger does under production-shaped load, what broke, and what
was fixed. Releases: baseline `1ba097d`, fixes rolled as `08a47ec` and
`60b659f`. Fleet driven from the control VM (`10.160.0.100:9090`), 2–4 workers.

Suites:

- `internal/registry/usage_stress_test.go` — the ledger's writers, raced
- `internal/server/usage_stress_test.go` — the metering hooks, raced
- `internal/apiv1/usage_paging_test.go` — the public read path's pagination
- `tests/usage-stress.ts` — real VMs on the fleet, 11 scenarios + an opt-in
  deep-ledger scale probe (`USAGE_STRESS_DEEP=1`)

Volume: **1437 sandboxes / 1503 intervals** across four fleet runs. The final
run on the rolled release passed **909/909** checks; the scale probe churned 900
sandboxes back to back with **zero create failures**.

| run | release | sandboxes | intervals | checks |
|---|---|---|---|---|
| baseline | `1ba097d` | 162 | 184 | 688/693 |
| after labels fix | `08a47ec` | 164 | 186 | 693/693 |
| deep-ledger probe | `08a47ec` | 900 | 900 | 10/10 |
| final | `60b659f` | 211 | 233 | 909/909 |

## Findings

### 1. Every sandbox the v1 API created billed with no labels (fixed)

**Symptom.** `metadata` was empty on the first interval of every sandbox, so no
interval could be attributed to a team, project or run. Measured on the fleet
before the fix: **0 of 24** intervals from a burst of v1 creates carried a
label; **162 of 184** intervals across the whole baseline run were
unattributable.

**Root cause.** The ledger snapshotted labels when the interval opened, and the
v1 adapter creates a sandbox on a worker and only then `PATCH`es metadata onto
the row (`internal/apiv1/handler.go` `create`). The interval was always open
before the labels existed. It looked like it worked because a later pause/resume
opens a new interval from the sandbox row, which by then does have them — so
interval 1 was empty and 2+ were correct.

**Fix.** `registry.SetOpenUsageMetadata`, called from the worker's
`public-fields` handler — the single funnel both the create-time annotation and
a later user `PATCH` go through. An **open** interval tracks the sandbox's
current labels; a **closed** one is history and is never rewritten, which is
what keeps a later `PATCH` from rewriting a bill.

**Verified.** 48/48 burst intervals labelled, 900/900 in the scale probe, and
the relabel case pinned: the closed interval still reads `first` while the
accruing one reads `second`.

### 2. A sandbox that moved hosts lost its labels (fixed)

**Root cause.** The durable hibernation record (`hibRecord`) carried name,
resources, expiry and ports but not `metadata`/`source`, so a sandbox adopted
onto another host came back unlabelled — and billed unattributably for the rest
of its life. The move happens when a host was lost, which is when reconciling
usage against sandboxes matters most.

**Fix.** The record carries labels and provenance; `adopt` restores them before
the clone wake, so the first interval on the new host already has them.

### 3. A sandbox that moved hosts restarted its line-item numbering (fixed)

**Root cause.** `OpenUsageInterval` derives the next sequence from the local
table, and an adopting host has never seen the sandbox — so it billed
`<sandbox>:1` again. The ledger's internal ids stayed unique (they carry the
host), but the public API presents a line item as `<sandbox>:<sequence>` and
documents it as unique. Two real intervals claimed to be the same charge, and a
consumer deduping on that id would drop one.

**Fix.** The freeze records how far numbering got (`hibRecord.usage_seq`), the
adopting host installs it as a floor (`usage_seq_floor`), and the floor is
pruned along with the intervals it protected. Records written before this
restart numbering as they used to.

### 4. `GET /v1/usage` could not page past 1000 intervals (fixed)

**Symptom.** Totals counted every interval in the window; pagination stopped at
1000 and there was no way to tell the missing rows from usage that never
happened.

**Root cause.** The v1 adapter paginates over the rows the ledger returned but
requested no particular number of them, so the ledger applied its default cap of
1000 while `next_page_token` walked past it.

**Fix.** The page being served sizes the fetch (`offset + page_size + 1`,
clamped to the ledger's 5000-row maximum). Past that maximum the response still
reports `coverage.truncated`, which is a limit a caller can see.

**Verified.** With 1389 intervals in the fleet ledger, paging reached **1389 of
1389** over 14 pages; before the fix it reached 1000.

### 5. The sampler cried "missed close" during ordinary churn (fixed)

The warning fired for every open interval found without a live VM, including
ones something else had legitimately closed in between — which races on every
hibernation. It now fires only when the close actually landed, so the message
still means what it is for: billing that would otherwise run away.

## What held up

Everything else. Across the four fleet runs and the local suites:

- **No double-billing.** One open interval per sandbox at all times; racing
  teardowns (destroy + pause + TTL + VM-exit + sampler, fired together) elect
  exactly one winner, and the host's billable counters credit exactly once.
- **No runaway intervals.** 0 intervals left accruing after teardown, across
  1437 sandboxes. The sampler closes an interval whose VM is gone.
- **Pause bills nothing.** Frozen sandboxes billed 0 additional seconds over
  12 s of being paused; every pause/resume cycle opened exactly one new
  interval, with no two intervals for a sandbox overlapping in time.
- **Billed ≤ observed wall clock**, on every sandbox, every scenario.
- **End reasons are truthful**: the TTL reaper closes as `expire`, the idle
  reaper as `hibernate`, an explicit delete as `destroy`, a drain as `shutdown`.
- **Allocated resources are what is billed**, including overrides
  (1×512, 2×2048, 4×4096 all matched) — and `vcpu_seconds` /
  `memory_mib_seconds` always equal resources × duration on the same row.
- **Consumed CPU stays inside the interval's physical ceiling** and never goes
  negative, including across a cgroup-counter reset.
- **Money does not depend on pagination**: identical totals at page sizes 1, 7,
  100 and 5000, and a settled sandbox's totals did not move once under 20 s of
  concurrent fleet churn and six concurrent readers (0 read failures).
- **Usage outlives the sandbox**: `?sandbox_id=` answers for deleted sandboxes;
  the id-routed route 404s and its message points at the one that can answer.
- **Crash recovery never bills the outage** — intervals are truncated to their
  last heartbeat, and sub-second sandboxes still leave a spoolable row.
- **Windows select by overlap** and never leak work from outside them.

## Known limits (by design, not defects)

- One response holds at most 5000 rows; past that `coverage.truncated` is set
  and totals still cover everything selected.
- Whole-second resolution: a sub-second sandbox bills $0 but still produces a
  row.
- `GET /v1/usage` reads **live hosts only**. Usage from a worker the MIG deleted
  lives in the durability bucket, which is the billing record of truth.
- Consumed CPU is recorded, never billed — CPU is deliberately oversubscribed.
- A selected interval is reported and totalled **whole**, never clipped to the
  window, because `cpu_usec` is one counter that cannot be apportioned.

## Reproducing

```bash
go test ./internal/registry/ ./internal/server/ ./internal/apiv1/ -race

# on the control VM
cd ~/web-sandbox/tests
SANDBOX_API_URL=http://10.160.0.100:9090 SANDBOX_API_KEY=$GATEWAY_TOKEN \
  npx tsx usage-stress.ts                      # 11 scenarios, ~160 s
USAGE_STRESS_DEEP=1 USAGE_STRESS_SCENARIOS=l … # 900-sandbox scale probe, ~7 min
```

Artifacts on the control VM: `~/usage-stress-{before,after,deep,final}.{log,json}`.
