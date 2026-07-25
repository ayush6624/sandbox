# Why UFFD — from a wake trick to a memory substrate

Status: **explainer / orientation.** Read this first, then
`docs/uffd-roadmap.md` (phase-by-phase status) and `docs/uffd-b2-design.md`
(the GCS chunk-source design). Ground truth for scale-to-zero context is
`docs/scale-to-zero.md`.

This doc explains, from the beginning and in plain terms, what UFFD is, why we
built it, why it's **off by default**, and why we're building on it anyway. The
short version: UFFD looked like a wake-latency optimization, got measured on the
fleet, and *lost* for today's workload — but its real value was never latency.
It's that the memory of a frozen sandbox can be sourced from *anywhere*, and
that's the foundation for cross-host wake, memory overcommit, and clone dedup.

---

## 1. The setup: hibernate and wake

Each sandbox is a Firecracker **microVM** — a tiny Linux virtual machine that
boots in a couple of seconds. People start work in one, then walk away. We don't
want an idle sandbox holding a whole machine's worth of RAM while nobody's using
it, so we **hibernate** it:

- Pause the VM, snapshot it to disk (Firecracker writes the guest's entire RAM
  to a **memory file** — ~1 GB for a 1 GB guest — plus a small VM-state file),
  kill the live `firecracker` process, and flip the registry row to
  `status=hibernated`. Tap/IP go back to the pools; the host port stays reserved
  so wake-on-connect still works. (See `internal/server/hibernate.go`.)
- When someone touches the sandbox again — an API call, or just a TCP connection
  to its forwarded port — we **wake** it: reload the snapshot and resume exactly
  where it left off.

The entire UFFD story is about one question in that last step:

> **When we wake a VM, how do we get its ~1 GB of memory back into the guest?**

---

## 2. Two ways to restore memory

### The File backend (what we ship today)

Before the VM resumes, Firecracker maps the whole memory file back into the
guest's address space. The guest doesn't run until its memory is present.

*Analogy: download the whole movie before pressing play.*

### The UFFD backend (userfaultfd)

`userfaultfd` is a Linux kernel feature that lets a normal user-space program
handle page faults for a region of memory. Firecracker supports it as a snapshot
memory backend: on restore, instead of loading the memory file eagerly, FC
registers the guest's RAM with a userfaultfd in **MISSING mode** and resumes
immediately with that memory *empty*. The moment the guest touches a page that
isn't present, the vCPU blocks, the kernel sends a fault message to our handler,
and the handler fills that page in (via a `UFFDIO_COPY` ioctl) — then the guest
continues. Memory is paged in **lazily**: only the pages the guest actually
touches, only when it touches them.

*Analogy: start streaming, buffer as you watch.*

Our handler lives in `internal/vm/uffd_linux.go` (plus platform-independent math
in `internal/vm/uffd.go`, unit-tested on macOS). `vm.RestoreUFFD` issues a raw
`PUT /snapshot/load` over the Firecracker API socket with
`mem_backend={backend_type:"Uffd", backend_path:<socket>}`. (The Firecracker SDK
v1.0.0 we pin has no `mem_backend` field, so this reuses the clone path's raw
`fcAPI` rather than the SDK's `WithSnapshot` helper.) FC connects to that socket
and passes us the userfaultfd file descriptor over `SCM_RIGHTS`; our handler
mmaps the (fully materialized) memory file read-only and services each fault by
copying the right bytes into the guest.

It's behind the config flag `"uffd_restore": true`, **default off**, and only
the same-identity hibernation wake (`wakeRestore`) uses it — the clone-path wake
still uses File.

---

## 3. The plot twist: File won

You'd assume streaming (UFFD) beats download-everything (File). We measured it on
a real fleet worker (1 GiB guest, local XFS, same-identity wake). It didn't:

| Backend | Warm wake | Cold wake |
|---|---|---|
| File | **~80 ms** | ~197 ms |
| UFFD (single-page faults) | ~110 ms | ~517 ms |

**File is faster.** Two reasons:

1. Our guests are small and the memory file is usually still **warm in the
   host's page cache** right after hibernation. So File's "eager load" isn't
   really reading a gigabyte off disk — it's just mapping already-cached pages.
   The movie was tiny and already buffered.
2. UFFD pays a **per-fault userspace round-trip**. At 4 KiB per page, faulting in
   a working set means thousands of block-fault-copy-resume cycles, and that tax
   (~30–50 ms in aggregate) exceeded what lazy loading saved.

The original design doc had assumed a ~1 s File baseline. That was wrong. The
fleet said so, and we committed `uffd_restore: false` as the fleet default
(commit `cad1284`).

**Conclusion: wake latency is the *weakest* use of UFFD, and the one case where
it loses.** For the current small-guest workload, keeping UFFD off is correct.

Two bugs the fleet run caught that were invisible off a real Firecracker+KVM
host (macOS builds the VM stub, so none of this runs locally):

- FC v1.15 reports `page_size_kib` in the UFFD message as **bytes** (4096), not
  KiB. Multiplying by 1024 produced 4 MiB "pages" and a wild offset.
- That bad offset under/overflowed a naive bounds check straight into a
  slice-index **panic** that crashed `serve`. Fixed by matching the fault to a
  region by aligned address, overflow-safe bounds arithmetic, and a `recover()`
  in the fault loop. (`pageSizeBytes()` in `uffd.go` now normalizes the unit.)

---

## 4. So why keep UFFD at all?

Because UFFD was never really about latency. It's about **where a page can come
from, and when.**

The File backend has a hard constraint: the memory file must be a **complete file
on the local host** before the VM can run. UFFD dissolves that constraint. The
fault handler is just code — it can source a page from a local mmap, a compressed
chunk, an object store (GCS), a peer host, or a zero-fill, and it decides *when*
to fetch. That makes snapshot memory a **virtualization substrate** instead of a
file you must possess in full up front.

That substrate unlocks four things the File backend structurally **cannot** do:

1. **Remote / chunked snapshot memory → cross-host scale-to-zero (Model B).**
   Serve pages on demand from GCS instead of a full local memory file. Any host
   can wake any sandbox without first downloading its whole gigabyte. This is the
   flagship prize — today hibernated sandboxes are **host-pinned** because their
   memory file lives on one host.
2. **Memory overcommit / density.** Reclaim idle guest pages back to the host (a
   balloon device does `MADV_DONTNEED`) and fault them back in via UFFD on next
   touch. This is the honest path to CLAUDE.md's open *"no memory overcommit"*
   item — a far bigger density lever than wake latency ever was.
3. **Page dedup + CoW across clones.** Every sandbox is a clone of the same
   golden snapshot, so clones share huge amounts of identical memory. UFFD lets
   them share one read-only backing; only dirtied chunks diverge and get
   re-uploaded (the BuildBuddy pattern).
4. **Live / postcopy migration.** Resume a sandbox on a target host immediately
   and pull its pages from the source on demand — rebalance the fleet or drain a
   host without cold restarts.

None of these are latency wins. That's the reframe: **UFFD is the foundation for
those four capabilities; the wake-speed benchmark was the one scenario where it
doesn't help.**

---

## 5. How the handler is built

The moving parts, so the roadmap below makes sense:

- **Fault loop.** One goroutine (pinned to an OS thread) per awake UFFD VM reads
  fault events off the userfaultfd and services them. It's driven by `poll()`
  (not a bare blocking `read()`) so it can wake on more than just faults — see
  teardown below.
- **`UFFDIO_COPY` alignment rule (locked in).** The destination address and the
  copy length must be page-aligned, but the **source pointer need not be**. That
  single fact is why a chunk decompressed into a heap buffer is a legal fault
  source — it's the enabler for compressed/remote pages.
- **Fault-ahead.** Instead of copying one 4 KiB page per fault, the handler
  copies a **128 KiB window** in one `UFFDIO_COPY`, amortizing the round-trip.
- **`pageSource` interface (the key seam).**
  `type pageSource interface { at(off, length uint64) ([]byte, error); close() error }`
  (`internal/vm/uffd_source.go`, kept untagged so it unit-tests on macOS). The
  handler asks `src.at(...)` for bytes and copies them in; it doesn't care where
  they came from. Implementations:
  - `localSource` — a zero-copy subslice of the mmapped local memory file
    (today's default when UFFD is on).
  - `chunkedSource` — indexes the memory image into fixed chunks, maps a fault to
    (chunk index, offset-in-chunk), materializes a chunk on first touch via an
    injectable `load(idx)`, caches it, and returns a subslice **clamped to the
    chunk end** (a straddling run returns short and the tail refaults into the
    next chunk, keeping every copy length page-aligned). Single-flights concurrent
    loads of the same chunk and prefetches the next N chunks under a semaphore.
  - `UFFDChunkSource` — a `chunkedSource` whose `load` is: local disk chunk cache
    → GCS `GetBytes(chunks/<sha256>)` → gunzip → write-through to the cache.
- **Kill-on-unserved-fault.** FC waits **forever** on a fault the handler never
  answers, so a swallowed fault hangs that guest silently. A `fatalOnce` on the
  handler, armed by `RestoreUFFD`, **SIGKILLs Firecracker** if `src.at()` returns
  an error — the guest stops cleanly instead of hanging. `recover()` in the fault
  loop keeps `serve` alive, but killing the VM is the actual correctness fix.
- **Deterministic teardown.** A blocking `read()` on the uffd does **not**
  reliably wake when Firecracker exits, so the fault goroutine would hang and leak
  its 1 GiB mmap + fd per wake. The loop now `poll()`s a set that includes a
  **stop eventfd** which `close()` signals on FC process-exit (`cmd.Wait`, which
  *is* reliable — POLLHUP on the uffd was not, on the fleet kernel). The
  memory mapping is unmapped by the fault goroutine only *after* FC exits, so a
  page copy can never race the unmap.

---

## 6. The roadmap (phases A–D)

Each phase is independently shippable and measurable. Full status and commit
trail live in `docs/uffd-roadmap.md`; this is the orientation-level summary.

### Phase A — make local UFFD competitive ✅ done
Fault-ahead (128 KiB window) brought warm wake from 109–132 ms down to
**80–86 ms = parity with File** (commit `1a1874b`). It's a tie, not a win — a
small page-cache-warm guest gives lazy loading nothing to save — but "not worse"
was the bar, so the rest of the plan can build on it. Working-set prewarm was
attempted here and **parked for Phase B**: snapshotting a UFFD VM faults in the
*entire* guest through the handler (FC reads every page to write the new memory
file), so naive recording captured the whole guest, not the startup set. The fix
(a seal-before-snapshot signal) landed in B3. Two durable wins kept from this
phase: the `poll()`-driven loop and the mmap/fd leak fix.

### Phase B — remote / chunked page source ✅ built & fleet-verified (default off)
The real prize. Sub-steps:

- **B0** — extract the `pageSource` seam (pure refactor). ✅
- **B1** — `chunkedSource` with a local `ReadAt`-backed loader; selected by
  `uffd_chunk_kib` (0 = mmap `localSource`, the default). ✅
- **B2** — GCS chunk source. On a **full** hibernation freeze, the memory image
  is chopped into chunks, gzipped, and uploaded content-addressed as
  `chunks/<sha256>` (**dedup** via a known-set + `Exists`; all-zero chunks are a
  never-stored sentinel), alongside a positional manifest
  `hib/<id>/manifest.json` written **last** as the commit marker. On wake with
  `uffd_chunk_gcs` on, only `chunkedSource.load` changes: local cache → GCS →
  gunzip → write-through, feeding B2a's prefetch + single-flight + kill-on-fault.
  vm stays free of the GCS client — the source is injected via
  `RunOptions.UFFDChunks`. Codec is gzip (zero new deps; the manifest carries a
  `codec` field so zstd is a later format-compatible swap). ✅
  - **B2c fleet measurement (2026-07-22):** a full freeze uploaded 512 chunks /
    1 GiB (~93 unique non-zero ≈ 50 MiB gzip, ~419 zero sentinels; a re-freeze
    deduped 15–22 chunks — CoW works). A cold-chunk-cache wake fetched every
    touched chunk from GCS: per-fault **p50 ≤ 2 µs** (prefetch/cache hits),
    **p99 ≤ 65 ms**, **max ~99 ms** (cold `GetBytes` + gunzip of one 2 MiB chunk),
    end-to-end wake **~1.4–2.1 s**. On the *same host* this is slower than local
    File (~0.2 s) — **as expected**: it pays GCS fetches the local backends avoid.
    Its win is **cross-host (B4)**, where File must download+rebase the whole
    1 GiB before resuming. B2 is proven correct and lazy end-to-end.
  - Bug fixed here (commit `f68733c`): the teardown stop-eventfd described in §5 —
    without it the fault goroutine leaked per wake because POLLHUP didn't fire on
    the fleet kernel.

- **B3** — working-set prewarm, done right ✅ (commit `4e9b424`, fleet A/B'd).
  Chunk-granular: `chunkedSource` records the chunk indices the guest **faults**
  in `at()` (not the prefetch path — the working set is what the guest touched).
  The **seal** fixes the Phase A pollution: `hibernate()` calls
  `vm.SealUFFDRecording(m)` *before* Pause+Snapshot so the snapshot's whole-guest
  read isn't recorded, then persists `vm.UFFDWorkingSet(m)` to
  `hib/<id>/workingset.json`. Next chunk wake fetches that set and spawns
  ≤`prefetch` bounded workers that bulk-fetch those chunks in the background as
  the guest resumes — turning a cold fault-storm into a warm cache.
  - **Fleet A/B (2026-07-22):** two cold-cache wakes of the same sandbox (chunk
    cache `rm`'d between). Baseline `faults=512 mean=2753µs p99≤65ms max=104ms`,
    wake **1.709 s**. With prewarm @`prefetch=4`: `mean=1294µs max=82ms`, wake
    **1.198 s** — ~30% faster wake, ~2× lower mean fault. But p99 stayed in the
    ≤65 ms bucket: 4 workers can't warm the 78-chunk working set before the guest
    faults its tail.
  - **`prefetch=32` crash — diagnosed, fixed, verified** (commit `54a71e2`).
    Root cause from `/tmp/firecracker-<vmid>.log` (the gotcha: FC restore logs go
    to `/tmp`, not the task dir): FC **panics on resume** —
    *"available virtio descriptors N > queue size: 256"* — during its "artificially
    kick devices" step, which reads the virtio rings out of UFFD-served guest
    memory. Isolation: `prefetchAhead@32` (post-resume) is fine and `prewarm@4` is
    fine; only `prewarm@32` crashes → the trigger is high-concurrency prewarm
    racing FC's **resume-time** ring reads. Fix: don't launch prewarm in the
    `newChunkedSource` constructor (which ran at/before `LoadSnapshot`); store the
    indices and have `RestoreUFFD` call `startPrewarm()` **after** the load+resume
    API returns, so prewarm races only the guest's own faults.
  - **Net B3 result:** at adequate concurrency (≥~32) prewarm **collapses the
    cold-wake fault tail ~500× (p99 65 ms → 128 µs)**. Recommended enable config:
    `uffd_restore` + `uffd_chunk_gcs` true, `uffd_chunk_prefetch ~32`.

- **B4 — cross-host wake ⏭️ next.** The architectural piece and the first real
  UFFD *win*. Make the VM-state file + chunk manifest durable in GCS, let a
  *different* host pull the state and serve memory via the GCS source, and extend
  gateway placement/routing to wake a sandbox off its origin host. Everything
  through B3 proves the source works; B4 is where it beats File — because File
  would have to download+rebase a full gigabyte before it could even start.

### Phase C — density via overcommit (balloon + UFFD) — not started
Wire a virtio-balloon device, handle `UFFD_EVENT_REMOVE` (a balloon reclaim
`MADV_DONTNEED`s a range that stays uffd-monitored and later refaults — the
handler must **zero** it with `UFFDIO_ZEROPAGE`, not re-serve stale file bytes),
and add a reclaim policy that balloons idle guests' cold pages back to the host.
**Gated on the balloon device**: FC registers the restore uffd in MISSING mode,
so the handler can page memory *in* but cannot reclaim resident pages itself —
balloon is the reclaim half, UFFD is only the page-in half. Biggest density win,
biggest lift.

### Phase D — hardening & scale — not started
One `epoll` fault loop across all VMs (instead of a goroutine + OS thread per
VM — fine at tens, not thousands); a separate **jailed handler process** (UDS +
mem file inside the jail, FC pid via `SO_PEERCRED`) for multi-tenant isolation;
kill-VM-on-unrecoverable-fault + watchdog; metrics (fault rate, page-in latency,
working-set size, chunk hit/miss).

---

## 7. Correctness gotchas (do not relearn the hard way)

- **FC waits forever on an unserved fault.** A page source that fails after
  retries must **kill the VM**, never silently skip (`fatalOnce` → SIGKILL).
- **`UFFDIO_COPY` needs `dst` + `len` page-aligned, but not `src`.** Heap /
  decompressed chunk buffers are valid fault sources because of this.
- **`page_size_kib` is BYTES in FC v1.15**, not KiB. `pageSizeBytes()` normalizes
  it. Match faults to regions by aligned address (no underflow).
- **Never block the single fault thread on a bare network RTT.** Pipeline fetches,
  keep multiple in flight, lean on B3 prewarm. Measure **p99** page-in, not mean.
- **`recover()` alone isn't enough** — a swallowed fault still hangs the guest.
  Pair it with kill-on-fault-failure.
- **Snapshotting a UFFD VM faults in the whole guest** through the handler —
  seal recording before Pause+Snapshot (`SealUFFDRecording`) or the working set
  is garbage.
- **Only the full-freeze path is chunked.** Diff freezes hold only dirty pages
  and wake locally; the chunk source falls back to the local memory file when a
  manifest is absent.

### Operational notes
- Chunk cache dir: `/mnt/sandbox-data/snapshots/chunkcache` (`rm` it via
  `nomad alloc exec` to force a cold-cache wake for measurement).
- FC restore VM logs: `/tmp/firecracker-<vmid>.log` (**not** the task dir).
- Fleet access needs **both** gcloud reauth (for gsutil/GCS) **and** Tailscale
  SSH re-auth each session.
- `deploy-job.sh` git-adds nothing, but a blanket `git add -A` will sweep the
  `devbox-gcp.json` measurement flags into a commit — stage files explicitly.
- The fleet is currently on `98a7eaf` (UFFD off) with test artifacts cleaned up.

### Config flags
- `uffd_restore` (bool, default false) — use the UFFD backend for same-identity
  hibernation wake.
- `uffd_chunk_kib` (int, default 0) — chunk size; 0 = today's mmap `localSource`.
- `uffd_chunk_gcs` (bool) — source chunks from GCS on wake (implies chunked).
- `uffd_chunk_prefetch` (int, default 4) — background prefetch/prewarm workers;
  ~32 is where the cold tail collapses.

---

## 8. Sources
- Firecracker: [handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md),
  [snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- BuildBuddy: [Snapshot, Chunk, Clone: Fast Runners at Scale](https://www.buildbuddy.io/blog/fast-runners-at-scale/)
- Aquifer: [Hierarchical Memory Pooling with CXL and RDMA for MicroVM Snapshots](https://arxiv.org/pdf/2606.24079)

---

## TL;DR
UFFD lets a frozen sandbox's memory be paged in lazily, from a source the fault
handler chooses. As a wake-latency trick it **lost** to the eager File backend on
today's small, page-cache-warm guests — so it's **off by default**, correctly.
Its real value is that pages can come from **anywhere**: that's the foundation for
cross-host wake, memory overcommit, and clone dedup — none of which File can do.
The groundwork (Phases A–B3) is shipped and fleet-verified; prewarm at
`prefetch≈32` collapses the cold-wake tail ~500× (p99 65 ms → 128 µs). The next
milestone is **B4, cross-host wake** — the first scenario where UFFD clearly beats
what we run today.
