# B2 design — GCS chunk page source

Status: **design, for review**. Prereqs done: B0 (`pageSource` seam) and B1
(`chunkedSource` — indexing + cache + injectable `load(idx)`). B2 changes exactly
one thing behind that seam: `load(idx)` gains a local-cache → GCS path, plus the
hibernate side that produces those chunk objects. See `docs/uffd-roadmap.md`.

## Goal and the one measurement that justifies it

Wake a UFFD-restored guest **without first materializing its whole mem file** —
fault pages in lazily from GCS-resident chunks. This is the first point where
UFFD *beats* the File backend rather than matching it: File must download/rebase
the entire ~1 GiB mem image before `resume_vm`; the chunk source downloads only
the chunks the working set touches.

The number that decides success is **p99 page-in latency on a cache-dropped
wake** (not mean — a single slow chunk stalls the faulting vCPU). "Cache-dropped"
because hibernation is still host-pinned in B2: the local mem file physically
exists at wake, so to measure the cross-host case we must force GCS fetches
(§Measurement). B2 does NOT move wakes across hosts — that's B4. B2 proves the
*source* end to end on one host.

Non-goal for B2: eviction (B1's no-evict cache stands; a woken guest's chunks fit
in host RAM for a single guest), overcommit (Phase C), cross-host placement (B4).

## Bucket layout

Reuse the existing bucket (`SnapshotBucket`), add two prefixes:

```
chunks/<sha256-hex>                       # content-addressed compressed chunk, immutable, SHARED across all VMs
hib/<sandbox-id>/manifest.json            # ordered chunk list + geometry for one hibernation image
hib/<sandbox-id>/state.sz                  # FC state file (small; existing sparse-stream format)
```

Content-addressing `chunks/<hash>` is what gives **dedup + CoW for free**: two
snapshots that share a page share the chunk object; a re-hibernation only uploads
chunks whose content changed (a new hash), and unchanged chunks are already
present (an `Exists` check, or a local "known-present" set, skips the upload).
The golden base's chunks live under the same prefix, so every clone's unchanged
pages dedup against the base with no special-casing.

Chunk objects are immutable and never deleted while any manifest references them.
B2 leaks them on snapshot delete (GC is a B4/ops follow-up — cheap, and premature
refcounting would be the wrong complexity here); `deleteSnapshotObjects`-style
cleanup for `hib/<id>/` removes the manifest + state only. **Log the leak** so it
isn't mistaken for full cleanup.

## Manifest format (`hib/<id>/manifest.json`)

```json
{
  "version": 1,
  "mem_size": 1073741824,      // total image bytes (defines the last short chunk)
  "chunk_size": 2097152,       // fixed, a multiple of the guest page size (see B1)
  "codec": "gzip",             // see §Codec
  "chunks": [                  // index i covers image bytes [i*chunk_size, ...)
    {"hash": "ab12…", "clen": 51234},   // clen = compressed object size (for logging/prefetch budgeting)
    {"hash": "0000…zero", "clen": 0},   // all-zero chunk: sentinel, never stored/fetched (see below)
    …
  ]
}
```

- **Index → object is positional**: chunk `i` backs image offset `i*chunk_size`;
  the array order IS the geometry. No per-chunk offset field needed (fixed size).
- **All-zero chunks** (huge in a fresh guest) get a reserved sentinel hash and are
  never uploaded or fetched — `load(i)` returns a zeroed buffer directly. This is
  the cheap 80% of the "compress zeros away" win without `UFFDIO_ZEROPAGE` (which
  the roadmap correctly defers to Phase C, where its value is zero-on-refault).
- Manifest is written **last**, mirroring `meta.json`'s commit-marker discipline:
  a hibernation image with no manifest is simply not chunk-restorable, never
  half-restorable.

## Read path — `gcsChunkSource` (the only new `pageSource` wiring)

`newGCSChunkedSource(manifest, blob, cacheDir, localMem)` builds a `chunkedSource`
(reused verbatim from B1) whose `load(idx)`:

1. **Local disk cache** `cacheDir/<hash>` present? → read + decompress → return.
   (Survives across wakes on the same host; content-addressed so shared.)
2. **Zero sentinel?** → return `make([]byte, chunkLen(idx))` (Go zeroes it).
3. **GCS**: `blob.GetBytes("chunks/"+hash)` → decompress → write-through to
   `cacheDir/<hash>` (tmp+rename) → return.
4. On step 3 failure after `gcsblob`'s existing retry: **return an error**. The
   handler MUST escalate this to killing the VM (§Kill-on-fail), because a
   returned-but-unserved fault hangs the guest forever.

`localMem` (the still-present local mem file) is an optional **fallback/fast
path**: when set, `load` can `ReadAt` the chunk from it instead of GCS — this is
literally B1's local loader, and it's how a same-host wake stays fast when the
image is still on disk. The measurement config forces the GCS path by leaving
`localMem` unset (or pointing the source at a scratch host with the file removed).

The B1 clamp-to-chunk-boundary invariant is unchanged, so `UFFDIO_COPY` lengths
stay page-aligned. Decompressed chunk buffers are plain heap allocations — fine,
because **`UFFDIO_COPY` requires dst+len page-aligned but not src** (locked in
during B1; it's exactly what lets us decompress into a buffer).

### Hiding remote latency (the make-or-break requirement)

A bare GCS RTT (~tens of ms) on the fault path stalls the vCPU. Three mechanisms,
in build order of impact:

1. **Async prefetch window at chunk granularity.** On a fault for chunk `i`, kick
   off background fetches for `i+1 … i+P` (a few chunks) via a bounded worker pool
   with several in-flight requests, and block only on chunk `i`. Fetched chunks
   land in the cache; the guest's next sequential fault hits warm. B1's fault-ahead
   is *within* a chunk; this is *across* chunks.
2. **Working-set prewarm (B3).** Bulk-fetch the recorded working set (pipelined)
   around resume so the common pages are warm before the guest runs. B2 is only
   *viable* with B3 for a cold wake; B2 can ship and be measured without it (the
   number will be worse, and that's the honest baseline that shows why B3 matters).
3. **Concurrency-safe cache.** B1's `chunk()` already single-flights via the map
   under lock; extend it so a fault and a prefetch for the same idx don't both
   fetch (in-flight map of `idx → chan`), and so `at()` blocks on the in-flight
   fetch rather than issuing a duplicate.

The fault thread stays single (roadmap's epoll-scaling is Phase D); prefetch
workers are separate goroutines feeding the cache. `at()` for the faulting chunk
is the only synchronous wait.

## Kill-on-fetch-failure (correctness gate — do NOT ship B2 without it)

Today `copyRange` logs an `at()` error and returns; the fault stays unserved and
FC waits forever. Acceptable for `localSource` (never errors); **unacceptable for
a remote source**. Wiring:

- `uffdHandler` gets an `onFatal func(error)` set by `RestoreUFFD` to a closure
  that SIGKILLs the firecracker process (via the `rawMachine`/`Machine` handle it
  already holds) and records the wake as failed.
- In `copyRange`, an `at()` error (after the source's own retries) calls
  `h.onFatal(err)` exactly once, then returns. The `poll()` loop then sees
  `POLLHUP` (FC gone) and `serve()` exits cleanly, releasing the source — the
  existing teardown path, now reached deterministically instead of hanging.
- A failed wake surfaces exactly like a failed File-backend wake: 503 on
  agent-bound requests / close code 4503 on the shell WS. The sandbox stays
  `hibernated` and a retry can try again (chunks may be transiently unavailable).
- Keep the `recover()` — a handler *bug* still degrades to one failed wake, never
  a serve crash. `recover()` and `onFatal` are complementary: bug vs. clean fetch
  failure.

This is the roadmap's headline gotcha ("FC waits forever → a GCS fetch that fails
after retries must kill the VM"). It's a prerequisite to turning UFFD on anywhere,
so it lands in B2 even though the default stays off.

## Write path — chunking at hibernate

At hibernation freeze (after `Pause`+`Snapshot` produces the local mem file), a
background job mirrors `uploadSnapshot`'s discipline:

1. Walk the mem file in `chunk_size` strides. For each chunk: detect all-zero
   (fast `bytes`-style scan) → sentinel; else `sha256` the raw bytes.
2. For non-zero, non-present chunks: gzip-compress, `PutBytes("chunks/"+hash)`.
   Skip chunks whose hash is already present (local known-set first, then
   `Exists`) — this is the CoW/dedup step; a re-hibernation of a mostly-idle guest
   uploads almost nothing.
3. `PutSparse("hib/<id>/state.sz", statePath)` (reuse existing sparse stream).
4. **Last**, `PutBytes("hib/<id>/manifest.json", …)` as the commit marker.

Runs in the background like `uploadSnapshot` (failure logs and leaves the image
host-local — the existing local hibernation still works; B2 upload is additive).
Bounded by an `uploadTimeout`-style deadline. The dirty-chunk optimization can
lean on FC's diff bitmap later (like diff hibernation), but content-hashing every
chunk is correct and simple for B2 — measure upload time before optimizing.

## Codec decision: gzip now, zstd only if p99 says so

**Decision: gzip** (`compress/gzip`, the codec `gcsblob` already uses at
BestSpeed). Rationale:
- Zero new dependencies — the project deliberately keeps the tree pure-Go and
  small (CLAUDE.md; `gcsblob` doc-comment). zstd means adding `klauspost/compress`.
- The manifest carries `"codec"`, so switching later is a format-compatible change,
  not a migration — new images use the new codec, old ones keep gzipping.
- Decompress latency is on the fault path, so if p99 shows gunzip dominating (it
  can, at BestSpeed ratios), revisit with klauspost zstd (faster decompress AND
  better ratio). Make that call **with the p99 number in hand**, not speculatively.

The all-zero sentinel removes the biggest compression burden (sparse zero regions)
regardless of codec, which further weakens the case for a heavier codec up front.

## `gcsblob` additions

Minimal: none strictly required — per-chunk objects use existing `PutBytes` /
`GetBytes` / `Exists`, and gzip framing is trivial to do at the call site (or add
tiny `PutGzip`/`GetGzip` helpers next to the sparse ones). Chunk objects are
small (≤ chunk_size compressed), so whole-object GET is fine; no HTTP Range verb
needed. **This keeps B2's blast radius inside `internal/server` + the new
`gcsChunkSource`, not the transport.**

## Proposed build sub-order (each a commit, still B2)

Design-review this doc, then:

- **B2a — read path + kill-on-fail, fed by a LOCAL manifest.** Add
  `gcsChunkSource` + the `onFatal` kill wiring + the concurrency-safe cache +
  chunk-level prefetch. Generate the manifest/chunks from the local mem file at
  wake (no GCS yet) or a one-shot local tool. Proves the fault-over-chunks-with-
  escalation path with zero network variables. Unit-testable with a fake blob.
- **B2b — hibernate upload + GCS fetch + dedup.** Wire the write path, flip
  `load` to local-cache → GCS, content-hash dedup/CoW, all-zero sentinel.
- **B2c — measurement.** Cache-dropped wake p99 vs File backend on the fleet.

(This is the "thin slice first" shape under a design-first umbrella — the kill-on-
fail correctness gate lands in B2a, before any network flakiness can hide behind
it.)

## Measurement plan (B2c)

- **Setup**: one host, `uffd_restore=true`, chunked GCS source. Hibernate a
  representative sandbox (idle Node/pnpm devbox) → chunks+manifest in GCS.
- **Cold wake**: drop the local chunk cache AND the page cache
  (`echo 3 > /proc/sys/vm/drop_caches`), remove/ignore the local mem file so
  `load` must hit GCS. Wake via an agent-bound request. Record per-fault latency
  (add a histogram to the handler) and end-to-end wake time.
- **Baseline**: same sandbox, File backend, cache-dropped — this pays the full
  download+rebase before resume. Expect File to LOSE here (the whole point).
- **Warm wake**: caches intact — expect parity-ish with B1 local (sanity check we
  didn't regress the common same-host case).
- **Report**: p50/p99 page-in, wake-to-first-exec, GCS bytes fetched vs image
  size, chunk cache hit rate, upload time + dedup ratio at hibernate.

## Open questions for review

1. **chunk_size**: 2 MiB proposed (fewer objects, better compression, still 512×
   the 4 KiB page so clamp waste is negligible). 1 MiB if p99 wants finer prefetch
   granularity. Pick one to start; it's per-image in the manifest so it's tunable.
2. **Prefetch width P**: start 4 chunks in-flight? Tune against p99.
3. **Chunk cache location/quota**: under `SnapshotDir` (shares the XFS reflink
   domain). No eviction in B2 — is a size cap needed before B4, or defer with the
   leak-logging? (Proposed: defer, log.)
4. **Manifest for the golden base**: build base chunks eagerly at `ensureGolden`
   so the first-ever remote wake dedups against them, or lazily? (Proposed: eager,
   reuses the existing eager base-upload hook.)
