# Request-gated sandboxes (scale-to-zero per request)

Status: **exploration** — branch `explore/scale-to-zero`. No code yet; this is a
feasibility study + phased plan.

## The idea

Run a sandbox *only* while it's actively serving. A request arrives on a
forwarded port (nginx, an HTTP handler, whatever the guest exposes), we wake the
sandbox, serve it, and the moment it goes idle we freeze it back to disk. A host
then holds **thousands of dormant sandboxes** — each a real, addressable endpoint
— while paying RAM/CPU only for the handful actually mid-request. Lambda-style
scale-to-zero, but the unit is a full microVM, not a function.

## We are already ~70% there

The mechanism this needs mostly exists. It was built for *idle* hibernation; the
idea is to make the idle window ~0 and event-driven.

- **Wake-on-connect** — `portproxy.go` already binds every forwarded host port in
  userspace, and a connection to a hibernated sandbox's port wakes it
  (`dialGuest` → `ensureRunning` → `wake`) before dialing the guest. The listener
  stays bound across the freeze; that's the whole point of it being userspace and
  not DNAT.
- **Hibernation frees the expensive resources** — freeze releases tap + IP back
  to the pools and kills the VMM (zero RAM). Only the host port, a listener
  goroutine/fd, the registry row, and the on-disk snapshot survive. Hibernated
  rows survive server restarts (`reopenPortListeners`).
- **Diff snapshots** — a freeze against the golden base writes only pages dirtied
  since clone (`hibernate.go`, `diffBase`), so per-sandbox freeze cost and disk
  footprint are a fraction of full guest RAM. This is what makes "thousands"
  plausible on disk.
- **Memory admission** — `mem_budget_mib` is checked *inside* `registry.Wake`, so
  the number of *concurrently awake* sandboxes is already bounded; an over-budget
  wake returns `ErrMemExhausted` → 503 / close-code 4503. Concurrency safety for
  the "wake storm" case is partly free.
- **Activity pinning** — an open forwarded connection pins the sandbox running
  (`act.begin`/`done` bracket the connection's whole lifetime), and the idle clock
  starts when the *last* connection closes. That is exactly the "process is done"
  signal we want to trigger freeze on.

## The physics

Two numbers decide whether this is a product or a demo: **wake latency** (added
to the front of a request) and **freeze cost** (paid every time a sandbox goes
idle).

### Wake latency — today ~1s, and why

`wakeRestore` (same-identity path, the common case) does:

1. `CreateTap` — ~ms
2. `NewMachineFromSnapshot` + `Start` → **`LoadSnapshot` + `ResumeVM`** — the cost
   center. The pinned **firecracker-go-sdk v1.0.0 has no `mem_backend` field**
   (confirmed: `SnapshotLoadParams` exposes only `MemFilePath`), so restore is the
   **eager File backend** — the guest's memory is faulted from the file up front.
   A diff freeze also pays `materializeHibMem` first (reflink base + sparse
   overlay of dirty pages; reflink is ~instant on XFS, the overlay write is real).
3. `waitForAgent` — polls sandboxd `/health` on a **200 ms** tick (up to 30 s).
   sandboxd was already running at freeze, so it answers on the first or second
   poll → ~100–300 ms of pure polling granularity.
4. `syncGuestClock` — one HTTP round-trip.

Net ≈ **1 s** (matches the fleet-validated "178 ms freeze / 1 s wake" note). That
is fine for an occasionally-hit endpoint, **too slow for an HTTP hot path**.

### Freeze cost — cheap, but churns

A diff freeze is ~178 ms and writes only dirty pages. The danger isn't a single
freeze, it's **thrash**: a sandbox taking 1 req/s, frozen immediately after each,
freezes+thaws every second — constant snapshot writes + reloads, disk I/O
amplification, wasted CPU. "Immediately shut off" must really mean "shut off after
a short cooldown." The cooldown is *the* density/latency dial.

### Density ceiling

A hibernated sandbox costs: 1 host port + 1 listener goroutine + 1 fd + 1 SQLite
row + its on-disk snapshot. RAM: **zero**. Tap/IP: **zero** (returned on freeze).

- 10k listeners = 10k fds/goroutines — trivial (raise `LimitNOFILE`).
- 10k host ports — need a **much wider port pool**. `PORTS_PER_HOST` defaults to
  4× slots; scale-to-zero wants a 20k–40k-port range (e.g. 20000–60000) since the
  port pool is the ceiling on *total* (running + hibernated) sandboxes.
- **Disk is the real ceiling.** 10k × diff-snapshot (tens of MB for a lightly-used
  guest) ≈ hundreds of GB. Full (non-diff) snapshots would be unaffordable — diff
  hibernation is mandatory for this mode. Rootfs is reflink (~0 incremental on
  XFS).
- **Concurrently awake** is bounded by `mem_budget_mib`, not by the dormant count.
  The real shape is "thousands hibernated, tens–hundreds awake at once."

## Verdict

**Practical within a single host, with two engineering investments and two hard
constraints to respect.**

Investments: (1) **UFFD lazy paging** to get wake under ~200 ms; (2)
**cooldown/hysteresis** so the freeze trigger doesn't thrash.

Hard constraints (design *around* these, don't fight them):

- **You cannot freeze underneath an open TCP connection.** Freezing kills the VMM;
  a clone-path wake gives the guest a *new* IP. The guest's half of any live TCP
  connection is gone. The current pin semantics already prevent this (freeze only
  when zero forwarded connections are open) — but it means **HTTP keep-alive is the
  enemy of density**: an idle-but-open keep-alive connection pins the sandbox
  awake forever. Request-gated mode must bound/close keep-alive (short
  `keepalive_timeout`, or an L4 idle-connection reaper on the proxy). This is the
  sharpest limitation.
- **The guest must be idle-safe to freeze between requests.** Background cron,
  in-guest timers, persistent outbound connections, and websockets all break under
  freeze. The model fits request/response workloads; document it as a contract.

Multi-host adds a third item — see Phase 5.

## Plan (phased, each phase independently useful)

### Phase 0 — Measure with what exists (no code)
Create a sandbox with `hibernate_after_sec: 1`, start a trivial HTTP server in the
guest on :3000, curl the forwarded port repeatedly, and measure real wake latency
and freeze cost on the target host. Validates the loop end-to-end and gives us the
baseline the later phases must beat. **Do this first.**

### Phase 1 — Event-driven freeze + cooldown
- New per-sandbox mode on `POST /sandboxes`: `{"scale_to_zero": true,
  "cooldown_ms": N}` (default cooldown e.g. 5 s). Stored in the registry.
- When `act` inflight for a scale-to-zero sandbox drops to 0, arm a per-sandbox
  cooldown timer instead of waiting for the 30 s reaper tick. Freeze when it fires
  and the sandbox is still idle. Keep the reaper as a backstop.
- This is pure host-side logic on top of the existing `hibernate()` — no VM changes.

### Phase 2 — Trim the non-UFFD wake latency (~1 s → ~400–600 ms)
- On the **port-proxy wake path**, drop the sandboxd `/health` gate: dial the
  guest's *service* port directly in a tight connect-retry loop (10–20 ms). The
  service's listening socket is restored immediately on resume; we don't need
  sandboxd for HTTP forwarding. Keep `waitForAgent` for exec/shell wakes.
- Make `syncGuestClock` async (fire-and-forget) for port wakes — a static HTTP
  response doesn't need a corrected wall clock.
- Add a **wake-concurrency semaphore** (mirror `create_concurrency`) so a burst of
  first-hits queues instead of storming disk. `mem_budget` already bounds committed
  RAM; this bounds the restore *work*.

### Phase 3 — UFFD lazy paging (the big win: → sub-200 ms) — **implemented, behind a flag**
- Load snapshots with `mem_backend: {backend_type: "Uffd", backend_path: <sock>}`
  and a userspace page-fault handler that serves the mem file on demand. Resume
  returns before pages are in; the guest faults its small working set lazily.
- **SDK v1.0.0 can't express this.** We reuse the existing **raw-socket path**
  (`m.raw` / `fcAPI` in `machine_linux.go`, already used for clone
  `PUT /snapshot/create` and pause/resume) to issue `PUT /snapshot/load` with the
  UFFD backend, bypassing the SDK's `LoadSnapshot`.
- Bonus: slashes wake *I/O* for large guests — only touched pages are read.

**Landed:** `vm.RestoreUFFD` + the handler in `internal/vm/uffd_linux.go`
(receives the uffd via SCM_RIGHTS, mmaps the mem file, services `UFFDIO_COPY` per
fault). Wired into `wakeRestore` for the **same-identity** wake path, gated by
`"uffd_restore": true` in the config (default off = the eager File backend). The
fault-offset math is unit-tested (`uffd_test.go`); the handler's page source is a
plain local `mmap`, structured so a remote/GCS source (Model B, below) slots into
the same fault path later.

**Not yet:** the clone-path wake (`wakeClone`) still uses the File backend; UFFD
there is a follow-up.

**Fleet-verified 2026-07-20 — and the result overturned the premise.** Measured
on a worker (1 GiB guest, local XFS, `same_identity` wake), internal wake time:

| wake | UFFD | File backend |
|---|---|---|
| first (cold page cache) | 517 ms | **197 ms** |
| warm | 109–132 ms | **81–83 ms** |

**File is faster here, so the fleet default is `uffd_restore: false`.** Two
things the design doc got wrong for this workload: (1) File-backend wake is
already ~80 ms, not the assumed ~1 s — the mem file is small and page-cache-warm,
so the "eager" load is just mapping cached pages; (2) UFFD adds a userspace
round-trip per 4 KiB fault, ~30–50 ms of overhead across the resume working set.
UFFD only wins when eager load is genuinely expensive: **large guests, cold/
uncached mem files, or remote (GCS) memory** — i.e. Model B and big-VM cases, not
today's small warm guests. The code stays (correct, tested, panic-safe) behind
the flag for exactly those cases. Where UFFD actually pays off — remote/chunked
memory, overcommit, migration — and the best-practices-grounded plan for it is in
**`docs/uffd-roadmap.md`**.

Two real bugs the fleet run caught (neither reproducible off real Firecracker):
a page-size field (`page_size_kib`) that FC v1.15 populates in **bytes** (4096),
not KiB — the ×1024 made 4 MiB "pages" and a wild offset; and that offset
underflowing/overflowing past a naive bounds check into a slice-index **panic
that crashed the whole `serve` process**. Fixed: match the region by the aligned
address (no underflow), overflow-safe bounds, and a `recover()` so a fault
handler bug can never take down serve again.

### Phase 4 — Provision-dormant + density knobs
- **"Create hibernated"** fast path: create → snapshot → freeze without ever
  handing the VM out, so you can stamp thousands of dormant, port-bound endpoints
  cheaply. (Today every sandbox is born running.)
- Widen `PORTS_PER_HOST` (20k–40k), raise `LimitNOFILE`, document disk as the
  density ceiling and diff hibernation as mandatory.
- Consider tiering snapshot storage (hot on tmpfs, cold on XFS).

### Phase 5 — Multi-host ingress (only if exposed beyond one host)
Wake-on-connect is host-local: the port listener lives on the owning host, not the
gateway. A public "request hits nginx" needs an **L4 ingress keyed by sandbox id →
owning host's forwarded port** so the inbound connection lands on the host that can
wake it. The gateway already routes *id-scoped HTTP* to the owning host; forwarded
*ports* are not gatewayed today. This is the one genuinely new subsystem.

## Open decisions (need your call before Phase 1)

1. **Cooldown default & whether it's adaptive.** Fixed 5 s, or adapt to observed
   request cadence (keep hot sandboxes warm, freeze cold ones fast)?
2. **Keep-alive policy.** Bound guest `keepalive_timeout`, add an L4 idle-connection
   reaper, or just document that keep-alive defeats density?
3. **Scope.** Single-host first (Phases 0–4), or is a public multi-host ingress
   (Phase 5) in scope now?
4. **UFFD appetite.** Is sub-200 ms wake a requirement (→ Phase 3 is mandatory), or
   is ~500 ms acceptable (→ stop after Phase 2)?
