# B4 design — cross-host wake

Status: **design, for review**. Prereqs done and fleet-proven: B0–B3 (the
`pageSource` seam, the chunked local source, the GCS chunk source with
kill-on-fault, and chunk-granular working-set prewarm). B2c proved the GCS chunk
source is correct and lazy end-to-end but *slower on the same host* (it pays GCS
fetches the local backends avoid). B4 is where that source finally pays off:
**waking a hibernated sandbox on a host that never created it.** See
`docs/uffd-roadmap.md` (Phase B) and `docs/scale-to-zero.md` (Model B — this is
the multi-host enabler it names).

## Goal and the one measurement that justifies it

A hibernated sandbox is **host-pinned today**: its registry row, device-state
file, and rootfs live only on the creating host's local disk, and the gateway
routes to that host by heartbeat. If the host dies, the sandbox is unreachable
until (and unless) that exact host returns. B4 makes a hibernated sandbox
**wakeable on any live host**.

The number that justifies B4 is **wake-to-first-exec on a host that does not
have the sandbox locally**, versus the File backend doing the same thing. The
File backend must download and rebase the *entire* ~1 GiB mem image (plus state
+ rootfs) before `resume_vm`; the GCS chunk source resumes immediately and
streams only the working set (B3 prewarm collapses its tail). B2c already
measured the chunk-source side in isolation (cold-local-cache wake ~1.2 s with
prewarm@32, p99 128 µs). B4c measures the honest cross-host A/B: **chunk source
should win decisively here — that is the entire thesis of the UFFD roadmap.**

**Decisions (resolved 2026-07-22, from review):**
1. **Scope = dead-host recovery AND drain/rebalance across healthy hosts.** The
   gateway may move a hibernated sandbox off a *live* host (maintenance drain,
   load spreading), not only recover one off a dead host. This needs
   two-live-host fencing + a freeze-then-move handshake — see §Ownership and
   §Drain.
2. **Gateway stays GCS-free** (resolve-on-miss asks a live host). Rationale
   below; revisitable but the default preserves the gateway's core invariant.
3. **Diff freezes are cross-host wakeable too** (not just full freezes). The far
   host rebases the diff mem + diff rootfs onto the durable golden base — reusing
   the *exact* snapshot-diff durability machinery (`uploadSnapshot` /
   `ensureBaseLocal` / `materializeMem` / `materializeHibMem`). See §Diff.
4. **Add `gcsblob.PutBytesIfGenerationMatch`** (create-if-absent + generation-
   match). This is what makes the owner fence a true CAS — required now that two
   *live* hosts can contend during a drain, not merely a revived dead one.

Non-goals for B4 (explicit): memory overcommit (Phase C), a single epoll fault
loop or a jailed handler process (Phase D), and (fully) GC of leaked chunk
objects (B4d stretch; B4a logs the leak like B2 did).

## What already exists vs. the four gaps

A cross-host wake needs, durably in GCS and reconstructable on a fresh host, the
four things that make a hibernated sandbox: **memory, device state, rootfs, and
metadata** — plus a fifth thing that is new to B4: **an owner, so two hosts
never both claim it.**

| Piece | Today | B4 gap |
|---|---|---|
| **Memory** | Full freeze chunks it to `chunks/<hash>` + `hib/<id>/manifest.json`; `gcsChunkSource` serves it to any host (`uffd_chunks.go`). | **None** — done in B2. This is the whole reason B4 is now cheap. |
| **Device state** (FC state file) | Local only (`snapshots/hib-<id>/…`). The B2 design *named* `hib/<id>/state.sz` but it was never uploaded. | **Upload it.** Small, sparse — reuse `PutSparse`/`GetSparse`. |
| **Rootfs** (guest disk) | Local CoW clone of the golden base; never uploaded. | **Upload it as a diff overlay** against the golden base (reuse `uploadSnapshot`'s `DiffExtents` + `PutRanges`). See §Rootfs. |
| **Metadata** (registry row) | Local SQLite only; the gateway learns the id from heartbeats but not the row. | **Upload a durable record** `hib/<id>/state.json` (row fields + port mappings + diff-base marker), written LAST as the commit marker. |
| **Owner** | Implicit: whoever has the local row. Fine when exactly one host ever has it. | **Explicit fence** so a revived original owner relinquishes a sandbox another host has adopted. See §Ownership — the hard part. |

## The identity reality: a cross-host wake is always the clone path

A hibernated sandbox's baked memory carries its **old** guest IP, and its old
tap belongs to the *creating* host's bridge and guest subnet. A different host
has its own tap pool, IP pool, and (possibly differently sized) guest subnet.
So a cross-host wake **cannot** be the same-identity plain restore
(`wakeRestore`). It is **exactly** the fan-out/`wakeClone` path we already have:
allocate a fresh tap/IP/port from *this* host's pools, resume on an unbridged
tap, MMDS-reidentify to the new identity, GARP, bridge. B4 reuses `wakeClone` →
`StartClone` → `finishClone` verbatim; it only changes **where the mem/state/
rootfs come from** (GCS instead of local disk) and **which registry the row
lands in** (this host's, via a `CreateRestore`-shaped insert).

This also means the closest existing precedent for B4's host-side work is
`handleRestore` (`snapshot.go`): `ensureSnapshotLocal` (pull from GCS if absent)
→ `CreateRestore` (fresh port + insert running row) → stage rootfs → resume →
`waitForAgent`. B4's "adopt" path is structurally the same, with the clone-path
resume and the GCS chunk mem source substituted in.

## Bucket layout (additive)

```
bases/<golden-id>/{mem.sz,rootfs.sz,complete}  # immutable base template — EXISTS (snapshot_gcs.go)
chunks/<sha256-hex>            # mem chunks (FULL freeze) — EXISTS (B2), shared/deduped across all VMs
hib/<id>/manifest.json         # mem geometry + chunk list (FULL freeze) — EXISTS (B2)
hib/<id>/workingset.json       # prewarm set — EXISTS (B3)
hib/<id>/mem.diff.sz           # NEW (DIFF freeze): dirty-page mem overlay vs the base golden mem
hib/<id>/state.sz              # NEW: FC device-state file (sparse stream)
hib/<id>/rootfs.sz             # NEW: rootfs overlay vs golden base (diff extents), or full-sparse/chunked for cold-boots
hib/<id>/record.json           # NEW: durable registry record; written LAST = commit marker
hib/<id>/owner                 # NEW: ownership fence (host id + epoch), CAS-written; see §Ownership
```

A **full** freeze's mem lives in `chunks/` + `manifest.json` (served lazily by
`gcsChunkSource`). A **diff** freeze's mem lives in `hib/<id>/mem.diff.sz` and
references `bases/<golden-id>/mem.sz`; the far host rebases it to a full local
file (`materializeHibMem`) and wakes off that (local mmap UFFD or File) — the
base is the bulk, is immutable, and caches once per host, so the diff transfer
is tiny. `record.json` names which mem form applies.

`record.json` (not `manifest.json`) is the cross-host commit marker: a sandbox
is cross-host-wakeable iff `record.json` exists. It carries everything
`CreateRestore` needs plus the pointers the wake needs: id, name, vcpus, mem_mib,
expires_at, hibernate_after_sec, base_snapshot_id, the diff-base golden id (if
the mem/rootfs are diffs), the chunk geometry codec, and the port mappings
(primary + `sandbox_ports`) so the adopting host rebinds the same guest→host
port intent. Base rootfs/mem templates already live under `bases/<golden-id>/…`
(`snapshot_gcs.go`) and are immutable, so a diff record just references them.

## Rootfs — the one genuinely new upload cost

Mem is already cheap (chunked + deduped). The rootfs is the new bytes B4 must
make durable, and there are two cases:

- **Base-derived sandbox** (hot-created clone; `BaseSnapshotID` set — the common
  case): upload only the extents that diverged from the golden base rootfs, via
  `DiffExtents` + `PutRanges`, exactly like a diff snapshot. A lightly-used
  devbox diverges by tens of MiB. The adopting host reflinks the (already-cached
  or GCS-pulled) base rootfs and overlays the extents — the `ensureBaseLocal` +
  `CloneFile` + `OverlaySparse` machinery already exists.
- **Cold-boot sandbox** (no base; `BaseSnapshotID` empty): no base to diff
  against, so upload the whole rootfs `PutSparse` (~hundreds of MiB sparse).
  Correct but heavy. **Proposal: B4a chunks the rootfs too**, reusing the exact
  content-addressed chunk machinery the mem already uses (`buildChunkManifest`
  is codec/geometry-agnostic) so cold-boot rootfs uploads dedup against the base
  and against each other. If that's too much for a first cut, fall back to
  full-sparse for cold-boots and log the cost (see Open Question 3).

The rootfs must be reconstructed at the **byte-identical absolute path** the FC
state file baked (FC reattaches the block device by path). The fleet uses one
config template across hosts, so the per-sandbox rootfs path scheme is
host-independent — the adopting host writes to the same path. This is a
correctness invariant to assert at adopt time, not a new mechanism.

## Ownership — the hard part (split-brain fencing)

Once host B adopts and wakes sandbox X (originally frozen on host A), B's SQLite
has X `running` and B heartbeats it → the gateway routes X to B. The danger:
**host A comes back.** `reconcile` skips hibernated rows, so A still has X
`hibernated`, re-binds its port listener, and heartbeats X → the gateway now
sees X on **both** A and B. Split brain: two VMs, one id, two divergent rootfs.

Because **drain/rebalance is in scope**, two *live* hosts can now legitimately
want the same id at overlapping moments (A owns X; the gateway drains A and
hands X to B while A is still up). So the fence must be a real compare-and-swap,
not just a revived-dead-host guard. Three tiers, cheapest first:

1. **The gateway is the sole dispatcher of off-host wakes / moves.** An adopt or
   a drain-move happens *only* when the single gateway decides it — on a route
   miss (owner gone) or an explicit drain. Two hosts never spontaneously race to
   adopt; the gateway serializes the decision. (Gateway HA is out of scope;
   there is one gateway.)
2. **An owner fence `hib/<id>/owner`** = `{host_id, epoch}`, monotonically
   increasing epoch, **CAS-written** (tier 3). Adopt bumps the epoch under a
   generation-match. **On restart AND on every heartbeat's route-claim,
   `reconcile`/registration honors the fence**: a host claiming X must hold the
   current fence; if the fence names another host with a higher epoch, this host
   **relinquishes** (drops its local row + port listener; the GCS artifacts are
   now the adopter's). This closes both "dead A revives" and "drained A is still
   live and mid-flight."
3. **`gcsblob.PutBytesIfGenerationMatch`** (create-if-absent via
   `Conditions{DoesNotExist:true}`, update via `GenerationMatch`). ~10 lines on
   the existing `storage` client. **Decided: add it** — with two live hosts able
   to contend, the CAS is load-bearing, not belt-and-suspenders: the loser of the
   epoch CAS aborts its adopt and returns a retryable error, so exactly one host
   ever ends up owning X.

The gateway itself stays **stateless and GCS-free** (Decision 2), preserving its
core invariant. Rationale even under drain/rebalance: the gateway never needs to
*read* the bucket — for a **drain** it tells the current live owner to freeze +
release, then dispatches `/adopt` to the target host (which does the GCS work);
for **dead-host recovery** it dispatches `/adopt` to a chosen host and lets that
host's GCS lookup 404 if the id was never made durable. Every GCS touch happens
on a `serve` host that already has the blob client + creds. Keeping the gateway
pure also means gateway restart stays trivially safe (it rebuilds routes from
heartbeats + the fence, holding nothing). Revisit only if the extra host
round-trip on orphan-resolve ever shows up in latency.

## Diff — making diff freezes cross-host wakeable (Decision 3)

Scale-to-zero density leans on diff freezes (only dirty pages hit disk), so B4
must make them durable too, not just full freezes. The good news: this is almost
entirely the **existing snapshot-diff durability**, retargeted at `hib/<id>/`.

- **Upload** (B4a): a diff freeze already carries a local `diff_base` marker
  (the golden id — `hibDiffMarker`). Push `hib/<id>/mem.diff.sz` (`PutSparse` of
  the sparse diff mem — data = dirty pages only) and `hib/<id>/rootfs.sz`
  (`PutRanges` over `DiffExtents(rootfs, base.rootfs)`), and record the base id
  in `record.json`. `ensureBaseUploaded(golden)` guarantees the base template is
  durable first (it already is for any host that built/pulled the golden). A
  diff freeze uploads **kilobytes-to-megabytes**, not the guest.
- **Wake** (B4b, far host): `ensureBaseLocal(golden)` (cached per host, or one
  GCS pull) → reflink base mem + `OverlaySparse(mem.diff)` = full local mem
  (`materializeHibMem`, unchanged) → reflink base rootfs + overlay rootfs
  extents → clone-path resume off the full local mem. No chunk source needed for
  diffs: the base is the bulk and it's local, so a File/local-mmap UFFD wake off
  the rebased file is already fast (B2c: local mem wake ~0.5 s). The GCS chunk
  source (lazy) remains the path for **full** freezes, where the whole image is
  only in GCS.

So the mem story bifurcates cleanly by freeze type — full → GCS chunks (lazy),
diff → base + tiny diff (rebase local) — and `record.json` tags which. Both
reuse machinery that already exists and is fleet-proven.

## Drain — moving a hibernated sandbox off a *live* host (Decision 1)

Rebalance/drain is a gateway-initiated, three-step handshake (no live-VM
migration — we move a *frozen* sandbox, which is the cheap case):

1. Gateway → **current owner A**: `POST /sandboxes/{id}/release` — A ensures the
   sandbox is hibernated (freeze it if running: only drain idle/freezable
   sandboxes; a pinned one is skipped and retried later), ensures its artifacts
   are durable in GCS (the B4a upload), then drops its local row + port listener
   **without** deleting GCS artifacts, and stops heartbeating the id.
2. Gateway → **target host B**: `POST /sandboxes/{id}/adopt` — B CAS-bumps the
   owner fence to `{B, epoch+1}` (A held `{A, epoch}`), reconstructs from GCS,
   and (for a drain we can adopt *as hibernated* — reconstruct the local row +
   rebind port listeners without resuming, so the sandbox stays frozen on B and
   wakes lazily on next access, matching its pre-drain state).
3. Gateway updates `route[id] = B`; A's fence loss means even if A's release
   half-failed, A relinquishes on its next reconcile/heartbeat (tier 2).

Draining a whole host = iterate its hibernated ids through this handshake, then
its running ones can be freeze-then-moved or left to drain naturally. Only
**freezable** sandboxes move; an actively-pinned one blocks that id's drain
(surfaced, retried) — you cannot move a sandbox with an open forwarded
connection (same hard constraint scale-to-zero documents).

## Gateway changes — resolve-on-miss, then dispatch

Today `handleProxyByID` 404s when `route[id]` is empty. B4 adds a resolve step
that keeps the gateway pure (no GCS, no durable state):

1. Route miss for id X → the gateway picks a live host (least-loaded, same
   bin-pack policy) and issues an internal **`POST /sandboxes/{id}/adopt`** to
   it. The chosen host checks GCS for `hib/X/record.json`; **404 if absent**
   (truly unknown id), else it performs the adopt+wake (below) and returns the
   reconstructed row.
2. On success the gateway records `route[X] = chosen host` immediately (like it
   does for a create) and proxies the original request through. The host's next
   heartbeat makes the route durable.
3. Bound it: one adopt attempt per request (with the create semaphore + queue
   already gating bring-up on the host), and cache negative lookups briefly so a
   storm of requests for a genuinely-dead id doesn't fan out to every host.

This reuses the gateway's existing pick/reserve/penalize/failover machinery —
an adopt that fails with a capacity error fails over to the next host exactly
like a create does.

## Host-side adopt path

`POST /sandboxes/{id}/adopt` (internal; host token, gateway-dispatched):

1. If the row is already local and running → return it (idempotent; lost race).
2. `acquireCreate` (shares the bring-up budget with create/restore).
3. Pull + verify `hib/id/record.json` (404 → not adoptable).
4. **Fence:** CAS-write `hib/id/owner` to `{this host, epoch+1}` (tier 3). Lose
   the CAS → someone else is adopting; return a retryable 409/503.
5. Stage rootfs locally at the baked path: reflink base + overlay `rootfs.sz`
   (diff) or pull `rootfs.sz`/rootfs-chunks (cold-boot). Pull `state.sz`.
6. Build the `gcsChunkSource` for mem from the manifest (with prewarm) — the
   already-shipped `gcsChunkSource`, just invoked on a host that never wrote it.
7. `CreateRestore`-shaped insert into THIS host's registry (fresh tap/IP/port
   from local pools; keep X's id, name, resources, expiry, hibernate window).
   Rebind port mappings from the record.
8. `wakeClone` (fresh identity, MMDS reidentify, GARP) with the GCS mem source.
   Kill-on-fault (B2a) is the safety net for an unservable chunk.
9. `waitForAgent` + `syncGuestClock` (the stale-clock step matters more here —
   a cross-host wake can be arbitrarily long after freeze).

Failure at any step rolls the local row back and leaves the GCS artifacts +
fence intact (a retry, or the original owner, can still wake it) — mirroring
`rollbackWake`/`rollbackPreVM`.

## Proposed build sub-order (each a commit, each measurable)

Design-review this doc, then:

- **B4a — durable record + state + rootfs upload (no wake change).** Extend the
  freeze path to push, for **both** full and diff freezes: `state.sz`, the rootfs
  (diff extents when base-derived, else full-sparse/chunked), `mem.diff.sz` for
  diff freezes (full-freeze mem already chunks via B2), and `record.json` (last,
  = commit marker). Purely additive durability; nothing reads it yet. Also add
  `gcsblob.PutBytesIfGenerationMatch` (unit-tested against the fake blob; used by
  B4b). Measure: extra upload bytes/time, rootfs dedup ratio, diff vs full split.
- **B4b — host-side adopt/release + fence (two-host, no gateway yet).**
  `POST /sandboxes/{id}/adopt` (reconstruct-from-GCS → clone-wake for a live
  wake, or reconstruct-as-hibernated for a drain), `POST /sandboxes/{id}/release`
  (freeze-if-needed + ensure-durable + drop-local), the owner-fence CAS, and the
  `reconcile`/registration relinquish-on-stale-fence. Testable by hand on two
  hosts (freeze on A, `adopt` on B; then a full A→B drain) before the gateway.
- **B4c — gateway resolve-on-miss + drain + measurement.** Route-miss →
  dispatch `/adopt`; a `/drain` control op → release-then-adopt handshake; honor
  the fence on route claims. Then the cross-host A/B vs File (dead-host recovery
  wake-to-first-exec) — the number that closes the UFFD roadmap's thesis — plus a
  drain-a-host timing.
- **B4d (stretch) — GC.** Refcount/GC leaked `chunks/<hash>` and `hib/<id>/*`
  on destroy. Deferred from B2; fold in if time allows, else its own follow-up.

## Correctness gotchas locked in (do not relearn the hard way)

- **Only FULL freezes are cross-host wakeable.** A diff freeze's mem holds only
  dirty pages against a *local* golden and is not chunked (hibernate.go already
  guards this). record.json is written only for full freezes; the gateway 404s
  a diff-only sandbox on a dead host (it was never made durable). Note this
  limitation — scale-to-zero density leans on diff freezes, so B4 durability and
  diff-freeze density are in tension (Open Question 3).
- **Rootfs path must match byte-for-byte** what the state file baked, or FC
  can't reattach the block device. Assert the fleet's path scheme is
  host-independent at adopt.
- **Fence epoch is monotonic and CAS-guarded**, or a revived owner with a
  stale-but-plausible fence could re-claim. Tier 3 (`gcsblob` CAS) is what makes
  this airtight; tiers 1–2 alone are best-effort.
- **Kill-on-fault (B2a) is mandatory here** — a cross-host wake faults over the
  network, so an unservable chunk must kill the VM, never hang the guest.
- The `syncGuestClock` step is **not optional** cross-host: the freeze→wake gap
  can be days, and NTP is blocked on some deployments (CLAUDE.md).

## Decisions — resolved 2026-07-22

All four review questions are resolved; the answers are recorded in the
**Decisions** block at the top and threaded through §Ownership, §Diff, and
§Drain:

1. **Off-host wake scope** → dead-host recovery **and** drain/rebalance across
   live hosts. Drove the CAS fence (Decision 4) and the §Drain handshake.
2. **Gateway↔GCS coupling** → gateway stays **GCS-free**; a live host does every
   GCS touch (§Gateway rationale). Marked revisitable if orphan-resolve latency
   ever bites.
3. **Diff-freeze durability** → diff freezes **are** cross-host wakeable; the far
   host rebases diff mem/rootfs onto the durable golden base (§Diff), reusing the
   snapshot-diff machinery.
4. **`gcsblob` CAS** → **add `PutBytesIfGenerationMatch`**; load-bearing now that
   two live hosts can contend during a drain.

Remaining implementation-level detail (not blocking B4a): cold-boot rootfs — chunk
it (content-addressed, dedups against the base like mem) rather than full-sparse,
so cold-boots are affordable; fall back to full-sparse + logged cost if chunking
the rootfs slips the first cut.
