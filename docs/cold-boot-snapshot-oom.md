# Full snapshots of jailed VMs are OOM-killed by their own cgroup

Status: **root cause identified, not fixed.** Reproduced on the production fleet
2026-08-03 at release `860406d`. Pre-existing — see [Attribution](#attribution).

## Symptom

Snapshotting a **cold-booted** sandbox kills Firecracker and **destroys the
sandbox**:

```
POST /sandboxes/{id}/snapshot
→ 500 {"error":"create snapshot: Put \"http://localhost/snapshot/create\": EOF"}
→ GET /sandboxes/{id} → 404  (row gone: "sql: no rows in result set")
```

`EOF` means the VMM died mid-request. The sandbox is then unrecoverable: the
freeze path's `vm.Resume` cannot reach the dead API socket, `watchMachine`
observes the exit, and `cleanupExitedMachine` reaps the row.

"Cold-booted" means **any sandbox created with a `vcpus` or `mem_mib` override**,
because an override cannot be served from the golden snapshot (Firecracker bakes
vcpus/mem into snapshots) and always takes the cold path.

### Three triggers, all destructive

1. `POST /sandboxes/{id}/snapshot` — 500, sandbox destroyed. Verified.
2. **Idle hibernation.** `configs/devbox-gcp.json` sets
   `hibernate_after_sec: 600`, so an override sandbox idle for 10 minutes is
   destroyed instead of frozen. Verified via the manual `POST .../hibernate`,
   which shares the same `s.hibernate` path as the reaper.
3. **Worker drain.** `shutdownAll` destroys explicitly when a freeze fails.

## Root cause

A **full** snapshot needs roughly **2× the guest's memory** inside the per-VM
cgroup: Firecracker reads the entire guest (making all of it resident) *and*
writes an equal-sized mem file, whose dirty page cache is charged to the same
cgroup. The jailer's leaf allows only:

```
memory.max = mem_mib + jailer_memory_overhead_mib     (156 on the fleet)
```

So the snapshot fails whenever `2 × mem_mib > mem_mib + 156`, i.e. whenever
**`mem_mib > 156`**. The cgroup OOM-kills Firecracker, and the API connection
closes as `EOF`.

### Evidence

Sweeping `mem_mib` on the fleet, everything else fixed:

| `mem_mib` | leaf `memory.max` | needs ≈2× | result |
| --- | --- | --- | --- |
| 128 | 284 | 256 | **OK** (`format=full`) |
| 256 | 412 | 512 | FAIL (`EOF`) |
| 512 | 668 | 1024 | FAIL (`EOF`) |
| 1024 | 1180 | 2048 | FAIL (`EOF`) |

The boundary sits exactly where the model predicts, between 128 and 256 —
`mem_mib` crossing `jailer_memory_overhead_mib`, not any absolute size.

**Why default sandboxes are unaffected:** they are golden-snapshot clones, so
`Server.diffBase` has an entry and their snapshots are **diff** — a few MiB of
dirty pages, which fits. Only the full-snapshot path is affected, and only cold
boots take it. Confirmed: a default sandbox snapshots, hibernates, wakes, and
destroys cleanly.

**Why it stayed hidden:** the `c0d0c0f` benchmark campaign's 25-cycle
pause/resume ran at 12 ms create — ready-pool hits, all default-source. Nothing
in the suite snapshots a cold-booted VM.

### The one inferred step

The OOM kill itself was not directly observed: the per-VM cgroup is removed with
the VMM, Nomad does not surface descendant-cgroup OOM events
(`nomad alloc status -stats` shows nothing), and the Firecracker log
(`firecracker-<vmid>.log`) lands in `os.TempDir()` — outside the alloc directory,
so `nomad alloc fs` cannot read it and the workers have no SSH key for the
control VM.

What IS established: the process dies (`EOF`), and the failure threshold matches
`memory.max` arithmetic exactly. To confirm directly, read `memory.events`
(`oom_kill`) on the serve task cgroup — it aggregates descendants — which needs
worker host access.

**Worth fixing regardless:** `LogDir` is never set, so every VMM log goes to
`/tmp` on the worker and is unreachable through Nomad. Pointing it at
`$NOMAD_TASK_DIR` would have made this a one-command diagnosis.

## Latent: a worker that must cold-build its golden cannot

`buildGolden` cold-boots a throwaway sandbox and takes a **full** snapshot of it
at template memory (1024 MiB) — the exact failing shape. Production never hits
this because workers **adopt** a baked golden (`golden snapshot ... adopted;
creates are hot` in every worker log), and `bake-image.sh` builds it under
`systemd-run --property=MemoryMax=4G`.

Unverified hypothesis, but it implies that a worker whose golden manifest is
missing or stale — the documented fallback — would **fail to cold-build one** and
serve every create on the slow cold path forever. It also suggests
`bake-image.sh golden` itself may be broken since the jailer landed. Both are
cheap to check and would be bad to discover during a scale-up.

## Fix options

**A. Fail closed (safe, stops the data loss).** Pre-flight the snapshot: if the
VM's cgroup cannot absorb another `mem_mib` of page cache, refuse *before*
pausing, so the VM is never put at risk. Converts silent destruction into a
clear error.
- User snapshot → 4xx, sandbox untouched and still running.
- Idle hibernation → freeze skipped, sandbox keeps running.
- Shutdown → still destroys (the existing designed fallback), but knowingly.

Cost, and it is a real product decision: override sandboxes then can never be
snapshotted or hibernated, so an idle one holds its slot until TTL or an explicit
delete instead of freeing capacity.

**B. Relax the cgroup for the snapshot window (the real fix).** The mechanism
already exists in the right shape: `snapshotWriteWindow`
(`internal/vm/jailer.go:542`) temporarily lifts the leaf's `io.max` for a
host-requested snapshot and restores it after. Extend that window to also raise
`memory.max`, since it is the same "this VM is doing one big privileged write"
moment.

The danger is explicit in the code's own comments: per-VM allowances are sized so
a fully admitted host cannot overcommit the parent task cgroup, because the
failure mode there is "OOM serve and every VM on the host at once". So this needs
bounding — serialize snapshot windows per host, require parent headroom ≥ the
guest size before granting one, and fall back to (A) when it is unavailable.

**C. Raise `jailer_memory_overhead_mib` ≥ the largest allowed `mem_mib`**
(config-only, immediate). Makes `memory.max` ≈ 2× guest so full snapshots fit.
Cost: admission charges the overhead per VM, so committed memory per slot goes
1180 → ~2048 MiB and density drops ~43% fleet-wide. Viable as an emergency
mitigation, not as the fix.

Recommended: **A now**, then **B** designed properly. Add a regression test that
snapshots a cold-booted VM at template memory — the gap that let this ship.

## Attribution

Not introduced by the usage-metering work deployed in the same release:

- The entire VMM-layer diff between the previously-deployed release (`dfc7afe`)
  and `860406d` is one `PreparedLaunch.CgroupLeaf` field, the same field on two
  machine structs, and five string assignments. Nothing touches launch, snapshot,
  pause, or resume logic.
- The freeze returns at `hibernate.go:496`; the metering hook is the next
  statement *after* that error block, so the failing path never reaches it.
- The failing block predates the work — last substantive change `e468115`
  ("Keep post-wake snapshots differential").

Caveat: this proves the code is pre-existing, not that the previous binary also
failed. An environmental change (a rebaked image carrying a different Firecracker,
a kernel bump) could have activated pre-existing code. Rolling one worker back to
`dfc7afe` and repeating the `mem_mib` sweep would settle that.
