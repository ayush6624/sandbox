# UFFD roadmap — from a wake trick to a memory substrate

Status: **plan**. Follows the shipped-but-default-off UFFD restore (see
`docs/scale-to-zero.md` and `internal/vm/uffd_linux.go`).

## The lesson from the first cut

We shipped UFFD as a *wake-latency* optimization and measured it on the fleet:
File-backend wake was already ~80 ms warm, UFFD ~110 ms — **File won**, because
the mem file is small + page-cache-warm, so eager load just maps cached pages
while UFFD adds a userspace round-trip per 4 KiB fault. Conclusion: latency is
the **weakest** use of UFFD, and the one case where it loses.

The point of UFFD is not "load the same local file lazily." It's that **the fault
handler can source a page from anywhere** — a remote object store, a compressed
chunk, a peer host, a zero-fill — and can decide *when* to load it. That's a
memory-virtualization substrate, and it's what unlocks the things this project
actually wants: multi-host restore, density, and migration.

## What UFFD uniquely unlocks (that the File backend cannot)

1. **Remote / chunked snapshot memory** — pages served on demand from GCS
   instead of a full local mem file. This is the **multi-host scale-to-zero
   enabler** (Model B in the scale-to-zero doc): wake any sandbox on any host
   without first downloading its whole memory. We already upload snapshots to
   GCS (`snapshot_gcs.go`); this makes the download page-granular and lazy.
2. **Memory overcommit / density** — reclaim idle guest pages (balloon →
   `MADV_DONTNEED`) and fault them back in via UFFD on next touch. This is the
   real "thousands per host" lever (CLAUDE.md's open "no memory overcommit"
   item), far more than wake latency ever was.
3. **Page dedup + CoW across clones** — every clone of the golden shares one
   read-only backing; only dirtied pages/chunks diverge and get re-uploaded.
   Cuts both host RAM and network (the BuildBuddy pattern).
4. **Live/postcopy migration** — resume a sandbox on a target host immediately,
   pull its pages from the source on demand. Fleet rebalancing / draining a host
   without cold restarts.

## Best practices (from Firecracker's own docs + production handlers)

- **The handler must never leave a fault unserved.** If it crashes or hangs mid
  fault, "Firecracker will wait forever" on that page. Our `recover()` keeps
  *serve* alive, but a swallowed fault still hangs that guest — so on an
  unrecoverable fault we must **kill the VM**, not just log. Add connect
  timeouts and a recycle/watchdog. [FC docs]
- **Handle `UFFD_EVENT_REMOVE`, not just PAGEFAULT.** When a balloon reclaims
  memory (`MADV_DONTNEED`), the range stays uffd-monitored and later refaults;
  the handler must **zero** those pages (`UFFDIO_ZEROPAGE`), not re-fetch stale
  file contents. Our loop currently ignores non-fault events — a correctness gap
  the moment ballooning/overcommit exists. [FC docs]
- **Serve zero pages with `UFFDIO_ZEROPAGE`.** Fresh/sparse regions are huge and
  zero; zeropage avoids reading/copying zeros from the file.
- **Fault-ahead.** FC's reference handler loads "the entire region the address
  belongs to" per fault; a tunable prefetch window (N pages around the fault, or
  the enclosing chunk) amortizes the per-fault round-trip that cost us in the
  A/B. This alone likely flips local UFFD to parity/better.
- **Isolate the handler.** Best practice is a **separate, jailed process** (UDS +
  mem file inside the jail, socket accessible only to FC + handler, cgroup
  limits), retrieving FC's pid via `SO_PEERCRED`. We run it in-process for now —
  fine for a trusted single-tenant fleet, but a deviation to revisit for
  multi-tenant. [FC docs]
- **Scale with one epoll loop, not a thread per VM.** Production handlers
  (Aquifer) multiplex every VM's uffd on a single epoll-driven thread. Ours
  blocks one goroutine (+ OS thread) per awake VM — fine at tens, not thousands.
- **Chunk + compress + dedup for remote memory.** Split the mem image into
  fixed chunks keyed by content hash, compress, store in the cache, fetch lazily,
  cache locally for reuse across VMs, and CoW dirty chunks (only re-upload the
  dirtied ones). [BuildBuddy]

## Plan (phased; each independently shippable and measurable)

### Phase A — Make local UFFD competitive (cheap, unblocks the rest)
Fault-ahead prefetch window + record the wake working-set and bulk-prewarm it on
the next wake (one big `UFFDIO_COPY` of exactly the touched pages, around resume).
Re-run the A/B; target UFFD ≤ File locally. This is the groundwork the remote
path reuses. (`UFFDIO_ZEROPAGE` moved to Phase C — its real value is
zero-on-refault under ballooning, not local latency.)

**Fault-ahead: DONE (commit 1a1874b), fleet-measured 2026-07-20.** A fault now
copies a 128 KiB window (`prefetchPages`) in one `UFFDIO_COPY` instead of one
4 KiB page. Warm wake went 109–132 ms → **80–86 ms, parity with File (~81 ms)** —
the per-fault round-trip regression is gone. It's parity, not a win: a small
page-cache-warm guest gives lazy loading nothing to save. Cold first-wake is
still ~520 ms vs File's ~197 ms.

**Working-set prewarm: ATTEMPTED, then PARKED for Phase B (commit 63094b0).**
Built record+persist+prewarm, but hit a design wall that makes naive recording
useless: **snapshotting a UFFD-restored VM faults in the ENTIRE guest memory
through the handler** — `hibernate`'s `PUT /snapshot/create` reads every guest
page to write the new mem file, so a page not already present faults to us. The
recorded "working set" therefore captures the whole guest (from the snapshot
read, not guest execution), and prewarm always trips its >50% skip. Recording
the true startup set needs a **seal-recording-before-snapshot** signal from the
hibernate path into the handler — real design work that belongs with the Phase B
remote source (where prewarm actually pays off and the recorded set is what you
prefetch over the network). Reverted the wiring; kept the two wins below.

Two things this increment DID land and keep:
- **`poll()`-driven fault loop** (commit 3759dcf): a blocking `read()` on the
  uffd does not reliably wake when Firecracker exits, so `serve()` never returned
  and its cleanup (mmap unmap, fd close) leaked one 1 GiB mapping + fd per wake.
  `poll()` sees `POLLHUP` on teardown and exits deterministically. Latent leak
  fixed; also the epoll-style shape the scaling best-practice wants.
- **fault-ahead** stays as above.

Lesson for when Phase B builds working-set properly: (1) stop recording before
the hibernate snapshot (seal signal), and (2) persistence must key off a
deterministic `serve()` exit — the `poll()` fix is the prerequisite.

### Phase B — Remote/chunked page source (the real prize: multi-host)
Introduce a `pageSource` interface behind the handler (the local mmap is one
impl). Add a GCS-chunk impl: chunked+compressed mem in the existing snapshot
bucket, lazy per-chunk fetch on fault, local chunk cache, CoW `.dirty` chunks,
dedup on re-upload. Wire the **clone-path wake** (`wakeClone`) and a new
"wake on any host" path to it. Requires async/pipelined fetch (never block the
fault thread on a bare network RTT) + working-set prewarm (done right this time)
so a cold remote wake doesn't fault-storm over the network.

**Starting point for the fresh session (de-risked ordering — build in this
order, each independently shippable + measurable):**

- **B0 — `pageSource` seam (pure refactor, no behavior change).** Extract the
  handler's page fetch into an interface:
  `type pageSource interface { at(off, length uint64) ([]byte, error) }`.
  Reimplement today's `mmap` path as `localSource`. `copyRange` becomes "ask the
  source for the bytes, then `UFFDIO_COPY`." Unit-test `localSource`. This makes
  everything below additive.
- **B1 — chunked local source.** Split the mem image into fixed chunks (start
  1–2 MiB), serve faults out of a chunk cache still backed by the local file.
  Proves chunk indexing + fault-over-chunks with zero network risk.
- **B2 — GCS chunk source (same host first).** At hibernate, upload chunks
  (compressed, content-hash keyed for dedup; only re-upload dirty chunks — CoW)
  to the existing snapshot bucket alongside a chunk manifest. On wake, fetch
  chunks lazily from local cache → GCS. Prove correctness + measure **p99** (not
  mean) page-in on a *cold* (cache-dropped) wake — this is finally where UFFD
  beats File, because File would download the whole 1 GiB first.
- **B3 — working-set prewarm, done right.** The Phase A attempt failed because
  the hibernate snapshot faults the WHOLE guest through the handler, polluting
  the recorded set. Fix: add a **seal-recording-before-snapshot** signal from
  `hibernate()` into the handler (stop recording once `Pause`+`Snapshot` begin),
  so the set is only guest-execution faults. Then bulk-prefetch that set (over
  chunks, pipelined) before/around resume. Reuse the parked bitset code from
  git history (commit eda7f63).
- **B4 — cross-host wake.** The architectural piece: hibernated sandboxes are
  host-pinned today (reconcile skips them; port listeners re-bind on the owner).
  Make the state file + chunk manifest durable in GCS, let a *different* host
  pull the state and serve mem via the GCS source, and extend gateway
  placement/routing to wake off-host. Do this LAST, once B2/B3 prove the source.

**Correctness gotchas locked in from Phase A (do not relearn the hard way):**
- FC waits **forever** on an unserved fault → a GCS fetch that fails after
  retries must **kill the VM**, never silently skip. Pair with the existing
  `poll()` loop + `recover()`.
- Never block the single fault thread on a bare network RTT — pipeline fetches,
  multiple in-flight, and lean on B3 prewarm. Measure p99.
- `page_size_kib` is BYTES in FC v1.15 (`pageSizeBytes()` handles it); regions
  matched by aligned addr (no underflow) — all already in `uffd.go`.

**First task on resume: B0** (the `pageSource` refactor) — small, safe, and it's
the seam everything else hangs off.

### Phase C — Density via overcommit (balloon + UFFD)
Wire a virtio-balloon device (FC supports it), handle `UFFD_EVENT_REMOVE`
(zero on refault), and add a reclaim policy that balloons idle guests' cold
pages back to the host, faulting them in on demand. This is the honest path to
overcommit CLAUDE.md calls for — UFFD is only the page-**in** half; balloon is
the reclaim half. Biggest density win, biggest lift.

### Phase D — Hardening & scale
Single epoll fault loop across all VMs; separate jailed handler process;
kill-VM-on-unrecoverable-fault + watchdog; metrics (fault rate, page-in latency,
working-set size, chunk hit/miss).

## Feasibility caveats (be honest up front)

- **Overcommit needs FC's balloon, not UFFD alone.** FC registers the restore
  uffd in MISSING mode; the handler can't reclaim resident pages itself
  (it has no handle on FC's address space). Reclaim = balloon (or privileged
  `process_madvise` into FC's mm — hacky). UFFD only serves the faults reclaim
  creates. Phase C is gated on the balloon device.
- **Remote latency must be hidden.** A per-chunk network RTT on the fault path
  stalls the faulting vCPU. Phase B is only viable with Phase A's prewarm +
  fault-ahead + multiple in-flight fetches. Measure p99 page-in, not just mean.
- **`recover()` isn't enough on its own** — a swallowed fault hangs the guest
  (FC waits forever). Pair it with kill-on-fault-failure (Phase D), which the
  current default-off flag makes non-urgent but is a prerequisite to turning
  UFFD on anywhere.

## Sources
- Firecracker: [handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md), [snapshot support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- BuildBuddy: [Snapshot, Chunk, Clone: Fast Runners at Scale](https://www.buildbuddy.io/blog/fast-runners-at-scale/)
- Aquifer: [Hierarchical Memory Pooling with CXL and RDMA for MicroVM Snapshots](https://arxiv.org/pdf/2606.24079)
