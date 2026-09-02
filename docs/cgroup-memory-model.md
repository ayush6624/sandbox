# How memory works on a sandbox worker

A from-scratch explanation of where a sandbox's memory actually lives, what the
"overhead" is, how it maps onto the physical machine, and why writing a snapshot
could kill a VM. Written to be read cold, without reading the code first.

If you only remember one thing: **RAM is charged to whoever touches it, including
RAM that is only passing through on its way to disk.** Almost every surprise in
this document follows from that.

---

## 1. What a cgroup is

A **control group** (cgroup) is a labelled box the Linux kernel puts processes
into, with limits attached to the box.

In cgroup v2 — what we use — each box is a *directory* under `/sys/fs/cgroup`,
and its limits are files you write numbers into:

| File | Meaning |
| --- | --- |
| `memory.max` | Hard memory ceiling. Cross it and something in the box dies. |
| `memory.high` | Soft ceiling. Cross it and the kernel throttles + reclaims. |
| `memory.current` | How much the box is using right now (read-only). |
| `memory.swap.max` | How much may be pushed to swap. We set **0** — no swap. |
| `cpu.max` | CPU quota per period. |
| `io.max` | Disk bandwidth/IOPS limits per device. |

Two properties matter for everything below:

1. **The kernel enforces this.** It is not bookkeeping or a hint. If a box is at
   `memory.max` and needs another page, the kernel either frees something or
   kills a process in that box.
2. **Boxes nest, and a child can never exceed its parent.** Putting a 10 GiB
   limit on a box inside a 4 GiB box does not grant 10 GiB.

---

## 2. The nesting on one worker

A production worker is an `n2-standard-16`: 16 vCPU, **64 GB RAM**, with
`SLOTS_PER_HOST=48`.

```
n2-standard-16 ─ 64 GB physical RAM
│
├── OS, Nomad agent, sshd, monitoring …          ~4 GiB   (not in a limited box)
│
└── nomad/…/serve task cgroup            memory.max ≈ 57 GiB
    │        └── every sandbox-related thing lives inside this one box
    │
    ├── sandbox-control leaf ── the serve process itself      ~2 GiB
    │
    ├── vm-a1b2…   memory.max = 1180 MiB      ← one microVM
    ├── vm-c3d4…   memory.max = 1180 MiB
    ├── vm-e5f6…   memory.max = 1180 MiB
    └── …up to 48 of them
```

The arithmetic that connects the boxes to the physical machine:

```
48 slots × 1180 MiB   =  56,640 MiB  ≈ 55.3 GiB    guests
                      +   2,048 MiB  ≈  2.0 GiB    serve
                      ──────────────────────────
                                     ≈ 57.3 GiB    ≤  64 GB machine  ✓
```

Nomad creates the task box; `serve` moves itself into a `sandbox-control` leaf
and then creates one leaf per VM underneath. The jailer launches Firecracker
directly into its leaf (`--parent-cgroup`), so a VM is inside its box from the
first instruction it executes — there is no window where it runs unconstrained.

---

## 3. Where the 156 MiB "overhead" goes

This is the most commonly misread number. Inside a single VM's box:

```
vm-a1b2…   memory.max = 1180 MiB
┌──────────────────────────────────────────────────┐
│  1024 MiB   guest RAM — what the guest sees      │
│                                                  │
│   156 MiB   Firecracker's OWN footprint:         │
│             · the VMM process (code, stack, heap)│
│             · virtio ring buffers                │
│             · device emulation state             │
│             · transient allocations              │
└──────────────────────────────────────────────────┘
             1180 MiB total
```

The guest believes it has 1024 MiB, and that is true from inside. But
Firecracker is *itself* a process on the host, and it needs memory to exist. The
box has to hold **both the guest's RAM and the emulator's own**.

So the overhead is not the guest's memory and not a safety fudge factor — it is
the emulator's cost of doing business. Hence `1180 = 1024 + 156`, and why
`MEM_PER_SLOT_MIB` defaults to exactly 1180.

Knobs, and how they relate:

| Knob | Where it applies |
| --- | --- |
| `mem_mib` (per sandbox) | the guest's share — 1024 by default |
| `jailer_memory_overhead_mib` | the emulator's share — 156 — sets each VM's `memory.max` at `mem_mib + this` |
| `MEM_PER_SLOT_MIB` | what capacity planning charges per slot — must be ≥ the two above summed |
| `mem_budget_mib` | the software admission ceiling — `SLOTS × MEM_PER_SLOT_MIB` |
| `SNAPSHOT_MEMORY_RESERVE_MIB` | parent-cgroup room not offered as slots; defaults to one slot charge so a dirty 1 GiB guest can snapshot at full occupancy |

### Two guards saying the same thing

Capacity is enforced twice, deliberately:

- **`mem_budget_mib` — the polite guard.** Before booting anything, `serve` sums
  the committed memory of running sandboxes and refuses a create that would
  exceed the budget: HTTP 503, `Retry-After`, and the gateway fails the create
  over to another host. Nothing dies.
- **The task cgroup's `memory.max` — the violent guard.** If the polite one were
  wrong, the kernel OOM-kills inside the task box. That means `serve` *and every
  VM on the host at once*.

You always want the polite one to fire first. That is exactly what
`CheckMemoryAdmission` verifies at startup, and why it refuses to start rather
than serve a host whose numbers don't add up. Its error message says it plainly:
*"at full occupancy the per-VM memory.max values sum above mem_budget_mib and
OOM the parent cgroup, killing serve and every VM with it."*

---

## 4. The page cache — the part that surprises people

When a process writes a file, Linux does **not** hand the bytes to the disk and
wait. It copies them into RAM — the **page cache** — marks those pages *dirty*,
returns success immediately, and flushes to disk in the background.

This is why writing a file feels instant. It also means:

> **A file write temporarily consumes RAM equal to the amount written, and that
> RAM is charged to the cgroup of the process that wrote it.**

Two kinds of page in a box behave very differently under pressure:

| Page type | Reclaimable? |
| --- | --- |
| Clean page cache (already flushed) | **Yes** — drop it, re-read from disk if needed |
| Dirty page cache (not yet flushed) | Not yet — must be written to disk first |
| Guest RAM (anonymous) | **No** — `memory.swap.max=0`, nowhere to put it |

That last row is important and it is a deliberate choice: guest memory can never
be reclaimed, so under pressure the kernel's only option is page cache.

---

## 5. Why snapshotting a VM could kill it

Take a **full** snapshot of a 1024 MiB sandbox. What happens:

1. The guest is **paused**. It is now a passive blob of RAM; its kernel is not
   running and it plays no part in what follows.
2. The host pre-creates an empty file inside the VM's chroot jail and hands
   Firecracker a *jail-relative* path (`/snapshots/output-1`). Firecracker cannot
   create files outside its jail, so the host has to provide one.
3. **Firecracker** — the host process — writes the guest's entire memory into
   that file. An ordinary `write()`.
4. The host `rename()`s the finished file into `/mnt/sandbox-data/snapshots/`.
   Same filesystem, so this is metadata-only: no copy, no extra I/O.

Nothing is written "inside" the guest, and there is no second export step. One
write, one rename.

The problem is step 3. That write creates up to 1024 MiB of page cache, charged
to the box that **already holds the guest**:

```
vm-a1b2…   memory.max = 1180 MiB   ←──── hard ceiling
┌────────────────────────────────────────────────────────┐
│ guest RAM (whatever it has touched)   ~300 MiB         │
│ firecracker itself                     ~30 MiB         │
│                                                        │
│ page cache of the snapshot file       ~850 MiB  ↑↑↑    │ ← growing
└────────────────────────────────────────────────────────┘
                          total ── 1180 MiB ── ✗ OOM kill
```

The kernel tries to reclaim, can't free dirty pages fast enough, and invokes the
cgroup OOM killer. Firecracker is the largest thing in the box, so it dies. From
the API's point of view the socket simply closes:

```
Put "http://localhost/snapshot/create": EOF
```

Note what was *not* the problem: the disk had plenty of free space, and the
finished file is only 1 GiB. **The constraint was RAM in transit, not storage.**

### Why the original reproduction affected only some sandboxes

| Sandbox kind | Snapshot type | Page cache burst | Outcome |
| --- | --- | --- | --- |
| Default (cloned from the golden snapshot) | **diff** — pages changed since the golden | usually a few MiB, but up to `mem_mib` | workload-dependent |
| Cold-booted (created with a `vcpus`/`mem_mib` override) | **full** — the entire guest | up to `mem_mib` | died in the original sweep |

A resource override cannot be served from the golden snapshot, because
Firecracker bakes vcpus/mem into a snapshot. So overrides always cold-boot, and a
cold-booted VM has no base to diff against, so its snapshot is always full.

Measured on the fleet, sweeping `mem_mib` with everything else fixed:

| `mem_mib` | box `memory.max` | result |
| --- | --- | --- |
| 128 | 284 MiB | **OK** |
| 256 | 412 MiB | died |
| 512 | 668 MiB | died |
| 1024 | 1180 MiB | died |

128 MiB survives for a structural reason: a 128 MiB guest **cannot touch more
than 128 MiB**, so its footprint can never exceed the 156 MiB of headroom above
its own size. Larger guests can, and do.

---

## 6. The fix: a warning line instead of an electric fence

The two memory limits differ in exactly the way that matters here:

| | When the box crosses it |
| --- | --- |
| `memory.max` | **Electric fence.** Reclaim, and if that fails, kill something. |
| `memory.high` | **Warning line.** Stall the process while reclaiming. Nobody dies. |

Because guest RAM is unreclaimable (no swap) and page cache is reclaimable, the
kernel under `memory.high` pressure targets *precisely* the snapshot's pages and
leaves the guest untouched. The targeting is free — we don't have to express it.

So for the duration of one snapshot, we draw a warning line just above where the
box currently sits:

```
1180 ┤ ══════════════════════ memory.max      (fence — no longer reached)
     │
 364 ┤ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─  memory.high  =  memory.current + 64 MiB
     │    ╱╲    ╱╲    ╱╲       ← write 64 MiB → throttle → flush → drop → repeat
 300 ┤ ──╱──╲──╱──╲──╱──╲───   guest's touched memory (never reclaimed)
     │
     └──────────────────────────────────────────────────► time
```

The write becomes a sawtooth: fill the margin, get throttled, writeback drains,
clean pages are dropped, continue.

Three deliberate details:

- **The line is derived from `memory.current`, not from `mem_mib`.** What matters
  is what the guest has actually *touched*; untouched guest pages map to the
  shared zero page and are never charged, so the configured size tells you
  nothing about real headroom.
- **It must sit ABOVE the current footprint.** With no swap, a line below the
  guest's own pages would throttle against memory that can never be freed — a
  hard stall, then the OOM we were avoiding.
- **It is installed and removed per snapshot**, in the same window that already
  lifts the VM's `io.max` write cap. A *permanent* `memory.high` would throttle
  any guest legitimately using its full RAM, forever — a much worse bug than the
  one being fixed.

That the `io.max` window already existed is a clue worth noticing: whoever wrote
it hit the adjacent symptom. A bandwidth-throttled write makes dirty pages pile
up faster than they drain — the same accumulation, seen from the I/O side. They
lifted the bandwidth cap and stopped there; the memory side went unaddressed.

### But a warning line cannot create room

A ceiling only helps if there is something reclaimable **and** somewhere to
reclaim into. A guest whose footprint already sits near its own limit has
neither: its pages are unreclaimable (no swap), and there is no slack left to
absorb the write. The kernel throttles without making progress, usage still
climbs to `memory.max`, and the OOM happens anyway.

That is exactly what the fleet showed. `memory.high` alone fixed 512 and 1024 MiB
sandboxes — those guests do not fill their configured RAM, so they carry real
slack — but a 256 MiB guest kept dying, because it does fill its RAM. The result
was non-monotonic, which is the tell that room, not pressure, was the missing
ingredient.

So for **every** snapshot the window also **raises `memory.max`** to
`current + write_size + margin`, and hands it back when the snapshot finishes:

```
        before                          during the snapshot
┌────────────────────┐          ┌────────────────────────────┐
│ memory.max   1180  │          │ memory.max   1440  (raised)│
│                    │          │ memory.high   434  ← ceiling│
│ guest        ~370  │          │ guest        ~370          │
└────────────────────┘          │ page cache   sawtooth ↕     │
                                └────────────────────────────┘
```

`memory.high` keeps actual usage down near the ceiling, so the raised limit is
mostly **unused insurance**. That is also why handing it back at the end is safe:
usage is already low, so lowering the hard limit reclaims nothing.

**Where does the extra room come from?** Only from what the task cgroup has not
already promised. Before raising, the code sums *every* per-VM leaf's
`memory.max` — their limits, not their usage, because an admitted-but-idle VM is
entitled to its full allowance at any moment — subtracts that plus serve's 2 GiB
reserve from the task limit, and raises only if the difference covers it. If it
does not, the snapshot is refused with everything left untouched. Overcommitting
the parent is the one outcome worth refusing service over: it OOMs serve and every
VM on the host at once.

The headroom calculation and each leaf-limit mutation are serialized, since two
callers must not both see the same bytes as free. The lock is released as soon
as each reservation is installed, so the snapshot writes themselves still run
in parallel.

### And a guard, because reserving is not always possible

On a host with no unreserved room, the reservation cannot be made — and
`memory.high` alone is not a proof. So the snapshot **fails closed**: it is
refused *before Firecracker is asked to do anything*. The VM is still alive and
paused at that point, so the caller resumes it and the sandbox survives.

An error instead of a destroyed sandbox is the whole point. Previously the freeze
path tried to resume a VM that was already dead, failed, and the row was reaped —
silent, unrecoverable data loss on a documented feature.

---

## 7. Performance

| | Effect |
| --- | --- |
| Guest, while running | **None.** `memory.high` exists only during a snapshot. |
| Guest, during the snapshot | **None.** It is paused; it cannot be slowed. |
| Lightly dirtied diff snapshots | The reservation is mostly unused insurance; writes remain parallel. |
| Heavily dirtied diff and full snapshots | Slower: disk-bound rather than RAM-bound — roughly the time to genuinely write the dirty layer (up to the whole guest) instead of "instant, flush later". |

The comparison for that last row is not "fast snapshot vs slow snapshot". It is
"slow snapshot vs destroyed sandbox".

One thing to watch: a slower freeze consumes more of `shutdownAll`'s 100 s drain
budget on worker shutdown. That budget covers freezing every sandbox on the host
in bounded parallelism, so it is worth re-measuring if full-snapshot sandboxes
ever become common.

---

## 8. Cheat sheet

```
physical RAM  ⊃  task cgroup (all sandboxes + serve)  ⊃  one VM's cgroup
                        ≈57 GiB                            1180 MiB
                                                          ├ 1024 guest
                                                          └  156 emulator

mem_budget_mib   = SLOTS × MEM_PER_SLOT_MIB   → polite 503 before booting
task memory.max                               → violent OOM if that's wrong

file write  →  page cache in RAM  →  charged to the writer's cgroup
memory.max  =  fence (kills)      memory.high = warning line (throttles)
guest RAM   =  unreclaimable (no swap)   page cache = reclaimable once flushed
```

This first came out of the cold-boot snapshot OOM: a FULL snapshot of a
cold-booted sandbox charged the guest's whole memory image to the same cgroup
leaf that already held the guest, and the kernel OOM-killed the VMM. Release
`ec5b70c` (2026-08-03) added the original full-snapshot guard. A dirty-working-set
benchmark on release `8284f21` (2026-08-26) then disproved its assumption that a
diff is always small: a 256 MiB continuously dirtied workload produced 270 MiB
of dirty snapshot page cache, reached the unchanged 1180 MiB leaf limit, and
OOM-killed Firecracker. `internal/vm/jailer.go` now applies the headroom
reservation to both Full and Diff snapshots while serializing only reservation
accounting, not their writes. On an isolated production-shaped `n2-standard-16`,
the candidate completed the same snapshot in 3.776 s, recorded 899
`memory.high` throttle/reclaim events and zero OOM kills, then passed 170/170
fan-out clone verifications. The Nomad task also retains one slot charge of
dedicated snapshot headroom so this remains available at ordinary slot
saturation rather than merely failing closed.
[usage-metering-plan.md](usage-metering-plan.md) covers the billing ledger,
which is how the bug was found.
