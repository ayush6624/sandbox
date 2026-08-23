# Warm templates: one entity, an admin knob, and a feedback loop

Status: implemented (2026-08-23). Snapshot consumers hold `snapshotLock`
shared, claims are template-exact, and the v1 batch issues chunked fanouts, so
creates from one snapshot are no longer serialized.

## Problem

A create from the built-in template is **7–16 ms** (ready-pool claim). A create
from a user template — the Node image, the Python image, the thing customers
actually boot in bulk — is **~700 ms**. Same mechanism, same artifacts, same
restore path. The entire difference is that the ready pool is hard-wired to one
snapshot:

```go
// internal/server/warm_pool.go buildWarmOne
snap := s.golden.Load()
if snap == nil {
    return errors.New("golden snapshot unavailable")
}
```

Nothing about a user template is slower. It is just second-class.

## The three entities are one entity

| Name | What it actually is |
|---|---|
| snapshot | a row in `snapshots` |
| golden | a row with `golden=1` — unique (`uniq_golden_snapshot`), undeletable, the pool's source, and baked onto the data-disk image via `golden.json` |
| template | a row produced by `template build`. **No marker whatsoever.** Schema-identical to a user snapshot |

So this plan does not add a third concept or a new id space. It replaces one
boolean with a role plus a target, which is what "let admins warm more than one
image" and "converge golden into templates" both reduce to.

## Goals

- Any snapshot may request N ready replicas per host, set by an operator.
- The built-in template keeps today's behavior by default (`warm_pool_size`, 8).
- Everything else defaults to **0** — explicit opt-in, no surprise residency.
- Per-template claim/miss metrics, so the knob has a feedback signal.

## Non-goals

- **No autotuner.** Targets are operator-set. Separate tooling can drive them
  later off the metrics this adds; guessing demand in-process is how you get a
  pool that fights real creates.
- No fleet-wide target distribution. Targets are **per host** (that is what the
  maintainer already is); the gateway aggregates for placement.
- No change to how snapshots or templates are built, stored, or made durable.
- Not baking multiple templates into the data-disk image. Separate, later, infra
  change (`golden.json` → a manifest list); see "Follow-on" below.

## Schema

Snapshots gain two columns:

```sql
ALTER TABLE snapshots ADD COLUMN role TEXT NOT NULL DEFAULT 'user';
ALTER TABLE snapshots ADD COLUMN warm_target INTEGER NOT NULL DEFAULT 0;
```

- `role`: `builtin` | `template` | `user`. `builtin` replaces the semantic load
  currently carried by `golden=1`. **Keep the `golden` column and
  `uniq_golden_snapshot`** — there is still exactly one host-built base image,
  it is still undeletable, and it still rides the image. `role` is what
  `GET /v1/templates` can finally list against.
- `warm_target`: ready replicas this snapshot wants **on each host**. 0 = none.

Sandboxes gain one column:

```sql
ALTER TABLE sandboxes ADD COLUMN warm_template_id TEXT NOT NULL DEFAULT '';
```

**This must NOT reuse `base_snapshot_id`.** That field has dirty-bitmap
semantics (CLAUDE.md: "Do NOT gate diffs on `sb.BaseSnapshotID` — it is never
cleared, and trusting it silently corrupts memory on restore"), and
`bringUpClone` deliberately only sets it when `snap.Golden`. A warm row built
from a non-golden template records `""` today, so the claim predicate below has
nothing to filter on without a new column.

## Code changes

### 1. `ClaimWarm` must filter by template — the correctness fix

Today (registry.go:1707):

```sql
SELECT ... FROM sandboxes WHERE status='warming' ORDER BY created_at LIMIT 1
```

With one pool source that is safe. With two, **it hands a Python sandbox to
someone who asked for Node.** This is a silent-wrong-answer bug, not a
performance issue, and it must land with the rest of this change or not at all.

```sql
SELECT ... FROM sandboxes
 WHERE status='warming' AND warm_template_id=?
 ORDER BY created_at LIMIT 1
```

`claimWarm`/`handleCreate` pass the resolved template id (the built-in's id when
the request names `default` or no source).

### 2. Pool maintainer iterates targets

`maintainWarmPool` reads `(template_id, target)` pairs from the
registry, read per-template `(ready, preparing)` inventory, and fill deficits.
`buildWarmOne(ctx, templateID)` replaces the `s.golden.Load()` read and calls
`createWarmFromSnapshot` with that snapshot — which needs no change, because
`ensureStagedRootfs` already made staging template-agnostic.

Two properties to preserve exactly as they are:
- A build stays `preparing` and unclaimable until every launch/readiness/security
  gate has completed; only `MarkWarmReady` promotes it.
- The maintainer polls as well as accepting kicks, so an unexpectedly dead ready
  VM is replenished.

### 3. Budget arbitration — mandatory, not optional

A ready VM is a real running VM: it holds a slot, a tap, an IP, and committed
memory against `mem_budget_mib`, and `registry.FreeSlots` already counts it. So
oversubscribed targets do not degrade gracefully — they consume the host's
advertised capacity and it stops taking creates.

`warm_pool_budget` defaults to `warm_pool_size`, preserving today's total.
The API rejects a sum of targets above the budget; the maintainer also enforces
the bound defensively, preserving the built-in pool first and then allocating
the remaining budget to explicit template targets.

### 4. Heartbeat and placement

Heartbeats retain the legacy aggregate `WarmReady int` and add
`WarmReadyByTemplate map[string]int`. New gateways use exact template affinity;
old gateways keep working during a rolling upgrade.

Placement ranking for a template-sourced create becomes:
1. host has a **warm row** for this template
2. host has the template **staged** (gateway already tracks `SnapshotIDs`)
3. host must pull it from GCS

Snapshot locality is already a preference rather than a pin, so this is an
extension of existing ranking, not new machinery.

### 5. Metrics — the feedback loop

Existing aggregate counters remain stable. `sandbox_template_warm_events_total`
adds `template` and `result` (`claim`, `miss`, `build_failure`) labels, while
inventory and targets are exported through `sandbox_template_warm_inventory`
and `sandbox_template_warm_target`.

### 6. Operator surface

- `PATCH /v1/templates/{id}` with `{"warm_target": n}`
- `sandbox template warm <id> <n>`
- `sandbox template list` — now possible, since `role` distinguishes templates
  from user snapshots

Bounds-check `warm_target` against `warm_pool_budget` at the API and say so in
the error; a 400 an operator can read beats a clamp they have to infer.

## Failure modes to get right

| Failure | Required behavior |
|---|---|
| Template deleted with a live pool | Delete already refuses while dependents exist (`SnapshotDependencyCount`). Ready rows must count as dependents, or drain them first — do not orphan warm VMs whose source is gone |
| Pool build fails repeatedly | Already handled: after a bounded window the host exposes normal clone capacity rather than staying unplaceable. Keep this per-template — one broken template must not make the host advertise 0 |
| Target set above budget | Clamp + log + 400 at the API. Never silently |
| Host restart | Ready rows are `warming`; shutdown destroys them (they are disposable) and the maintainer refills. Unchanged |
| Claim for a template with an empty pool | Falls through to the ordinary ~700 ms clone. This is the designed fallback — never weaken a gate to make it look faster |

## Test plan

Unit:
- `ClaimWarm` never returns a row from a different template (the correctness bug)
- Deficit fill is per-template and respects the budget clamp
- A build stays unclaimable until `MarkWarmReady`
- Metrics carry a `template` label

Fleet:
- Two templates × pool 4 each: claims hit at ready-pool latency for both
- Exhaust one pool, confirm the other is unaffected and the exhausted one falls
  back to a clone
- Re-run `snapshot-batch-bench.ts` for a warmed template — this is also the run
  that finally re-measures the batch row corrected in CLAUDE.md

## Explicitly rejected: pre-warmed SSH host keys from a key service

Considered and dropped. Ed25519 keygen is **~7 ms** of the 125–136 ms identity
phase (the ~1.2 s RSA-3072 that once dominated it is already gone). A key
service would buy ≤7 ms and would not touch the ~120 ms of round-trip and sshd
work that actually costs.

It also changes the secret model in a way a private VPC does not cover. Today a
host private key exists in exactly one place ever — inside the guest that owns
it, generated there, never transmitted. A key service means unissued private
keys at rest plus every key in transit, so one compromise impersonates every
sandbox it ever served; and the VPC is also where tenant guests run, making it
defense-in-depth rather than a confidentiality boundary. Uniqueness would become
a distributed allocation problem where a double-issue silently lets one tenant
MITM another.

If the unpooled path ever needs to be faster, have sandboxd generate a **spare
keypair in the background** after boot and swap it in: the key never leaves the
guest, uniqueness is free, no new service. A warm pool makes even that
unnecessary — for a pooled create, identity initialization already happened.

## Follow-on (separate changes)

1. **Bake promoted templates onto the data-disk image.** `golden.json` becomes a
   manifest list and `importGoldenManifest` loops. This is what removes the
   first-touch GCS pull from the scale-up path, so a freshly scaled host arrives
   already holding the hot templates. Same operational model as `sandboxd`:
   rebake + MIG roll.
2. **The ~189 s scale-out blackout.** At 1000 concurrent requests this dominates
   everything in this document by an order of magnitude. Pools help only when
   the hosts already exist.
3. **The per-clone floor** (~330–400 ms reidentify + ~130 ms identity). Worth
   attacking only after 1 and 2 — and note a pool does not shrink it, it moves
   it off the request path entirely, which is strictly better.

## What is not measured

The 7–16 ms vs ~700 ms gap is measured (release `c0d0c0f`). Everything this plan
claims about *multi*-template pools is projection: no two-template pool has ever
run. The CPU ceiling is the open question — `finishClone` already carries a
second reidentify margin because a starved guest misses the first, and N pools
refilling concurrently is more sustained bring-up pressure than one. Measure
refill-time reidentify latency before raising any concurrency knob.
