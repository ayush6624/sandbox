# Full snapshots of jailed VMs are OOM-killed by their own cgroup

Status: **root cause identified; fix implemented, pending fleet verification.**
Reproduced on the production fleet 2026-08-03 at release `860406d`. Pre-existing —
see [Attribution](#attribution). For the underlying memory model in plain terms,
see [cgroup-memory-model.md](cgroup-memory-model.md).

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

Firecracker writes the guest's memory into a file, and cgroup v2 charges that
file's **page cache to the writer's cgroup** — the same leaf that already holds
the guest. The leaf allows only:

```
memory.max = mem_mib + jailer_memory_overhead_mib     (156 on the fleet)
```

So the snapshot dies whenever

```
guest_touched_memory + dirty_page_cache_burst   >   mem_mib + 156
```

`memory.max` is a hard limit and dirty pages cannot be reclaimed until they are
written back, so the kernel invokes the cgroup OOM killer. Firecracker is the
largest process in the box, so it dies and the API connection closes as `EOF`.
The disk was never the constraint — RAM in transit was.

**Correction to an earlier reading of this:** it is *not* "a full snapshot needs
2× `mem_mib`". Untouched guest pages map to the shared zero page and are never
charged, so the guest contributes what it has *touched*, not its configured size.
The distinction matters for sizing a mitigation: the headroom needed is above the
guest's touched set (a few hundred MiB), not double the guest.

### Evidence

Sweeping `mem_mib` on the fleet, everything else fixed:

| `mem_mib` | leaf `memory.max` | headroom above the guest | result |
| --- | --- | --- | --- |
| 128 | 284 MiB | 156 MiB, but the guest can touch at most 128 | **OK** (`format=full`) |
| 256 | 412 MiB | 156 MiB | FAIL (`EOF`) |
| 512 | 668 MiB | 156 MiB | FAIL (`EOF`) |
| 1024 | 1180 MiB | 156 MiB | FAIL (`EOF`) |

The headroom is a constant 156 MiB in every row, so the boundary is not about
absolute size. 128 MiB survives structurally: a 128 MiB guest **cannot touch more
than 128 MiB**, so its footprint can never consume the headroom above its own
size. Every larger guest can, and does.

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

## Fix (implemented)

**`memory.high` for the duration of the snapshot, plus fail-closed.**
`snapshotWriteWindow` (`internal/vm/jailer.go`) already relaxed the leaf's
`io.max` write cap for exactly this operation; it now also installs a reclaim
ceiling at `memory.current + 64 MiB` and restores it afterwards.

Why this works: `memory.high` throttles and reclaims instead of killing, and
because `memory.swap.max` is `0` the guest's anonymous pages are unreclaimable —
so reclaim can only target the snapshot file's page cache, which is exactly the
intent. The write proceeds as a sawtooth: fill the margin, throttle, writeback
drains, clean pages drop, continue.

Three deliberate properties:

- **Derived from `memory.current`, not `mem_mib`** — what matters is the touched
  set, and this needs no knowledge of the guest's configuration.
- **Above the current footprint, always.** A ceiling below the guest's own pages
  would throttle against memory that can never be freed: a hard stall, then the
  OOM being prevented.
- **Installed per snapshot, never permanent.** A standing `memory.high` would
  throttle any guest legitimately using all of its RAM — a worse bug than this
  one. `prepareVMMCgroup` writes `memory.high = max` as the explicit baseline, so
  the file's absence is a hard error rather than a silently skipped guard.

**Fail-closed backstop.** `memory.high` makes the OOM far less likely but is not a
proof: if writeback cannot keep up, usage can still reach `memory.max`. So the
window refuses — before Firecracker is asked to do anything — when the guard
cannot be installed or there is less than one margin of headroom. The VMM is
alive and paused at that point, so the caller's resume succeeds and the sandbox
survives. An error instead of silent destruction is the point.

### Performance

- Guest while running, and during the snapshot: **no effect** (the ceiling exists
  only inside the window, and the guest is paused anyway).
- Default sandboxes: **no effect** — diff snapshots of a few MiB never reach the
  ceiling; freeze stays ~178 ms.
- Full snapshots: slower, becoming disk-bound rather than RAM-bound — roughly the
  time to genuinely write the file (order 1–3 s per GiB on the local XFS SSD)
  instead of "instant, flush later". The comparison is not fast-vs-slow snapshot;
  it is slow snapshot vs destroyed sandbox.
- Watch: a slower freeze eats more of `shutdownAll`'s 100 s drain budget. Worth
  re-measuring if full-snapshot sandboxes become common.

### Options considered and rejected

- **Raise `jailer_memory_overhead_mib`** above the guest's touched set
  (config-only, immediate). Works, but admission charges it per VM, so density
  drops proportionally fleet-wide. Kept as an emergency lever, not the fix.
- **Temporarily raise `memory.max`** instead of adding `memory.high`. Also works,
  but it must be bounded against the parent task cgroup, where overcommitment
  OOMs `serve` and every VM at once. `memory.high` needs no extra budget and
  carries none of that risk.
- **`O_DIRECT` or periodic fsync during the write** — both live inside
  Firecracker's code, so unavailable to us.

### Still to do

- Regression coverage on Linux that actually snapshots a cold-booted VM at
  template memory — the gap that let this ship. Current tests cover the guard's
  logic against a fake cgroup leaf, not a live VMM.
- Set `LogDir` (unset today, so VMM logs land in `/tmp`, outside the Nomad alloc
  directory and unreachable via `nomad alloc fs`). This would have made the
  diagnosis one command instead of an inference.
- Verify the `buildGolden` hypothesis above.

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
