# CLAUDE.md

## Project overview

Firecracker-based microVM sandboxes for development, exposed via a
local HTTP API over a Unix socket. Each sandbox boots Ubuntu 24.04 with Node 22,
pnpm, TypeScript, Python 3, and common build tooling (build-essential, git). It's
a bare sandbox — no app server runs on boot and guest ports are reachable only
after explicit exposure. e2b style — but self-hosted, on bare metal.

Multi-sandbox: each one gets its own tap, IP, and rootfs copy; host ports are
allocated only for explicit mappings.
State is in SQLite at `/var/lib/sandbox/registry.db`. The server (`sandbox serve`)
owns all running VMs in-process.

Every VM also runs `sandboxd` (cmd/sandboxd), a small in-guest HTTP agent on
`:8090` providing exec + file read/write. The host server proxies
`/sandboxes/{id}/exec|files|dir` to it over the bridge, and `POST /sandboxes`
blocks until the agent answers `/health` (~2 s total), so a created sandbox is
immediately usable.

## Build & run

```bash
make build            # Local build (uses stub on macOS — Firecracker calls return ErrLinuxOnly)
make build-linux      # Cross-compile bin/sandbox for linux/amd64 (pure-Go SQLite, CGO disabled)
```

Server + CLI (on a Linux host; both need root):

```bash
sudo ./sandbox serve --config configs/devbox.json    # daemon-ish; listens on /run/sandbox.sock
sudo ./sandbox doctor --config configs/devbox.json   # env validation
sudo ./sandbox up                                    # POST /sandboxes → prints JSON + URL
sudo ./sandbox list                                  # GET /sandboxes
sudo ./sandbox down <id>                             # DELETE /sandboxes/<id>
sudo ./sandbox hibernate <id>                        # freeze an idle sandbox to disk (next exec wakes it)
sudo ./sandbox rename <id> "my devbox"               # set a sandbox's display name ("" clears)
sudo ./sandbox exec <id> -- "node --version"         # run a command in the guest
sudo ./sandbox shell <id>                            # interactive PTY shell (WebSocket) in the guest
sudo ./sandbox ssh <id> [-- cmd...]                  # SSH in as the sandbox user (forwards :22 on first use)
sudo ./sandbox ssh-config <id>                       # print an ssh_config stanza (for plain ssh/scp/rsync)
sudo ./sandbox read <id> /path                       # file out of the guest → stdout
sudo ./sandbox write <id> /path [--from local]       # stdin/local file → guest
sudo ./sandbox ls <id> [/path]                       # list a guest directory
sudo ./sandbox install-agent --agent ./sandboxd      # bake sandboxd into base rootfs
sudo ./sandbox stop-server [--force]                 # SIGTERM (graceful) / SIGKILL the server
```

The non-serve commands are thin HTTP clients over the Unix socket. They need
`sudo` because the socket is mode 0600 and the binary needs the NOPASSWD
sudoers rule below. `install-agent` and `stop-server` are subcommands (not
scripts) specifically so they're covered by that NOPASSWD rule.

`serve` is self-healing on startup: it runs `EnsureNetwork` (bridge, sysctls,
NAT — survives host reboots), reconciles stale state (kills orphaned
firecracker processes, removes stale taps/rootfs/legacy-DNAT/DB rows), and
re-binds the port-proxy listeners of hibernated sandboxes.

## Remote deployment

```bash
make sync                              # build-linux + rsync bin/{sandbox,sandboxd} + Makefile + configs + scripts
make remote-install-agent              # sync + bake sandboxd into the base rootfs
make remote-doctor                     # ssh + run doctor
make remote-serve                      # ssh + run server (blocks)
make remote-up                         # ssh + create a sandbox
make remote-list                       # ssh + list
make remote-down SANDBOX=<id>          # ssh + destroy one
```

`sync` rsyncs the binaries so they land at `~/sandbox/sandbox` and
`~/sandbox/sandboxd` (not under `bin/`). All `remote-*` targets and the
README use `./sandbox`. Don't reintroduce `./bin/sandbox` in remote commands.

NOPASSWD sudoers (one-time, lets the CLI/server run as root without prompting):

```
ayush ALL=(ALL) NOPASSWD: /home/ayush/sandbox/sandbox
```

in `/etc/sudoers.d/sandbox` with mode `0440`.

## Deploying to the production GCP fleet

**Use the one command. Do not hand-run the individual steps.**

```bash
make fleet-rollout                     # roll HEAD onto the fleet, then verify it landed
infra/gcp/rollout.sh --fast            # rapid dev iteration (~15 MiB egress, gateway-only)
make fleet-status                      # where the fleet is right now (read-only)
infra/gcp/rollout.sh --dry-run         # what's stale + what would happen
infra/gcp/rollout.sh <sha>             # roll a previously published release (rollback)
make fleet-rollout SHA=<sha> ROLLOUT_ARGS=--skip-smoke
```

`rollout.sh` builds + uploads, deploys **only** the components whose *running*
release is stale, waits for convergence, and smoke-tests REST + the WebSocket
pty. It is idempotent — re-running on a converged fleet deploys nothing and
just re-verifies. Read its final summary; don't wrap it in extra polling.

Why this exists rather than the three steps it wraps: the previously documented
path (`make gcs-release && infra/gcp/deploy-job.sh`) rolls the **workers only**,
and `sandbox` is a *single binary* holding both `serve` and `gateway`, so
essentially every Go change ALSO needs `infra/gcp/control.sh deploy`. Forgetting
it half-deploys the fleet and fails silently: an old gateway re-encodes create
bodies through `client.CreateOpts` and drops fields new workers expect. The
gateway is therefore deployed FIRST (new gateway in front of old workers is the
benign direction). Convergence is judged on the gateway's own host inventory
(`release`, `release_compatible`, free capacity) — NOT `nomad alloc status` —
because an unwarmed host deliberately advertises `slots_free=0` and cannot take
a create yet; that wait is what keeps the smoke test from being racy. The smoke
covers REST *and* the pty because they authenticate and proxy differently, and a
REST-only check passed for a full release while the interactive shell was broken
fleet-wide.

Two mislabeling guards it will refuse on, both inherent to the older scripts:
`make gcs-release` compiles the **working tree** but labels it with HEAD's sha,
so a target that isn't HEAD is only ever rolled from an already-published
artifact, never rebuilt; and `control.sh deploy` has the same property with no
sha override, so rolling the **gateway** to anything but HEAD is refused up
front — `git checkout <sha>` first.

A change to `cmd/sandboxd` is NOT covered by this: the agent is image-pinned, so
it needs `infra/gcp/bake-image.sh bake && golden` and a MIG roll (see the golden
snapshot notes below). `rollout.sh` ships the `sandbox` server binary only.

**`--fast` is the dev-iteration path, and rollout latency is NOT on the fleet.**
Measured: golden is *adopted* (ms), the ready pool refills at ~1.5 s per VM in
parallel, and `shutdownAll` freezes at ~178 ms per VM 8-way parallel — so a
worker is serving again seconds after its task restarts. **Don't try to speed
rollouts up by discarding snapshots or sandbox data; that isn't where the time
goes.** The cost is egress from your machine plus control-plane work a code
change can't affect, so `--fast` strips and builds only `cmd/sandbox`
(30.5 MiB → 15 MiB), overlaps the GCS upload with the gateway push, restarts
only the gateway (`control.sh gateway` / `SECTIONS=gateway` in
`control-install.sh`) instead of reinstalling the four version-pinned services,
compiles once instead of twice, and polls every 2 s. It publishes as
`<sha>-dev` and always rebuilds, so a stripped dev artifact can never satisfy a
real `<sha>` release. NB the release prefix must contain **both** `sandbox` and
`sandboxd`: `serve.nomad.hcl` has a mandatory artifact stanza for each and
`run.sh` chmods both, so `--fast` copies `sandboxd` server-side from the
release the workers are already running (the pulled agent is never executed —
it's image-pinned — but a prefix missing it fails the roll).

## One-time host setup

```bash
sudo bash scripts/setup-firecracker.sh      # install firecracker binary
sudo bash scripts/setup-kernel.sh           # download Firecracker-compatible kernel
sudo bash scripts/build-devbox-rootfs.sh    # build /opt/fc/devbox-rootfs.ext4 (resumable, ~5 min)
sudo ./sandbox install-agent             # bake sandboxd into the rootfs (loop-mount, fast)
```

`setup-network.sh` still exists but is no longer required: `serve` runs
`provisioner.EnsureNetwork()` on every startup, which idempotently creates the
bridge, sets the sysctls, and adds the NAT/FORWARD rules. A host reboot just
needs `serve` restarted.

EnsureNetwork sets these critical host-wide knobs:
- `net.ipv4.ip_forward=1` — **required**: guest egress to the internet is routed + MASQUERADEd
- `iptables -t nat -A POSTROUTING -s <subnet> -o <host-iface> -j MASQUERADE` — **required**
  for guest egress (the guests' 172.16.x addresses aren't routable outside the host)
- `net.ipv4.conf.all.route_localnet=1` and the `-o br-fc MASQUERADE` rule — kept for
  back-compat with the retired DNAT port-forwarding scheme; harmless

Host:port → guest:port forwarding is NOT iptables DNAT anymore: it's a userspace TCP
proxy inside the server (`internal/server/portproxy.go`). The server binds each mapped
host port itself; every accepted connection counts as sandbox activity (resets the
idle-hibernation clock, pins the sandbox while open) and transparently wakes a
hibernated sandbox before dialing the guest (wake-on-connect).

## Code layout

```
cmd/sandbox/
  main.go              Root cobra command (wires all subcommands)
  serve.go             Boots the API server (EnsureNetwork + reconcile + listen); --gateway opts into fleet registration
  gateway.go           Boots the multi-host control plane (sandbox gateway)
  up.go                Thin client: POST /sandboxes
  down.go              Thin client: DELETE /sandboxes/<id>
  list.go              Thin client: GET /sandboxes (tabwriter output)
  rename.go            Thin client: POST /sandboxes/<id>/rename (display name)
  exec.go              Thin client: POST /sandboxes/<id>/exec; exits with the command's exit code
  shell.go             Interactive PTY: raw-mode stdin ↔ WebSocket /shell; relays SIGWINCH resizes
  files.go             Thin clients: read/write/ls over /files and /dir
  installagent.go      Loop-mounts the base rootfs, installs sandboxd + systemd unit
  stopserver.go        Finds `sandbox serve` PIDs via /proc, SIGTERM/SIGKILL
  doctor.go            Colored env checks (Linux, KVM, firecracker, kernel, rootfs, bridge, ip_fwd, API socket)
  helpers.go           Shared cfg/socket flags and Client constructor
cmd/sandboxd/main.go   In-guest agent: /health, /exec, /files (GET/PUT), /dir, /shell (PTY WebSocket), /clock on :8090
internal/agentapi/agentapi.go     Shared host↔guest protocol types + port constant
internal/config/config.go         JSON config + Defaults(); DisallowUnknownFields
internal/client/client.go         HTTP client: New(socket) for the local socket, NewHTTP(addr,token) for TCP+bearer (gateway/host)
internal/cluster/cluster.go       Host→gateway heartbeat protocol type
internal/gateway/gateway.go       Multi-host control plane: host registry, derived routing, placement, reverse-proxy, scatter-gather
internal/wsutil/wsutil.go         Minimal WS handshake+close-frame reject: delivers auth/routing errors on WS endpoints as close codes (4401/4404/…) browsers can see
internal/server/
  server.go           http.ServeMux on Unix socket; owns map[id]*vm.Machine; vmCtx lifetime
  proxy.go            Reverse-proxy to sandboxd (incl. /shell WebSocket via httputil) + waitForAgent readiness poll
  portproxy.go        Userspace host-port→guest-port TCP proxy: activity-tracking, wake-on-connect, listeners persist through hibernation
  heartbeat.go        When --gateway is set, periodically registers this host with the gateway
  reconcile.go        Startup cleanup of stale rows/taps/rootfs/orphan firecrackers (skips hibernated)
  hibernate.go        Idle hibernation: activity tracking, freeze-to-disk reaper, wake-on-access
  snapshot.go         Snapshot/restore/fan-out handlers (pause+snapshot, 1:1 restore, N clones)
  golden.go           Golden snapshot: built at startup, POST /sandboxes clones it (hot create)
internal/registry/registry.go     SQLite-backed registry; resource allocation (tap/IP/port from pools)
internal/provisioner/provisioner.go  Host-side ops: EnsureNetwork, rootfs cp, tap create/delete (+ legacy DNAT removal)
internal/vm/
  machine_linux.go    Firecracker SDK integration; ShutdownGuest, Wait, PID; captures stderr to firecracker-<vmid>.log
  machine_stub.go     Non-Linux stub matching the Linux signatures
  options.go          RunOptions + RuntimeConfig
configs/devbox.json   Default config (pools, bridge, paths, vCPUs/mem)
scripts/              Host setup shell scripts
```

## Architecture notes

- **Single long-running server.** `serve` owns every `*vm.Machine` in `machines sync.Map`.
  If the server crashes, firecracker children become orphaned and we can no longer ACPI-shutdown
  via the SDK. On the next `serve` startup, `reconcile()` kills any process whose
  `/proc/<pid>/comm` is `firecracker` for each registry row (guards against PID reuse), then
  releases tap, rootfs copy, legacy DNAT rules (pre-proxy hosts), and the row itself. Every
  row is stale by definition at startup, since VMs only live inside a running server —
  except hibernated rows, which reconcile skips and whose port listeners are then re-bound.
- **Multi-host is a gateway in front, not shared state.** `sandbox gateway` fronts the same
  API and fans out across hosts. Each host keeps its own SQLite + pools + `reconcile()`
  unchanged (a *shared* DB would break reconcile's "every row is stale" + PID checks). Hosts
  opt in with `serve --gateway <url> --gateway-token <tok> --listen <addr> --token <addr-tok>`
  and heartbeat (`internal/server/heartbeat.go`) their `{addr, token, slots, slots_free,
  warm_ready, sandbox_ids}` to the gateway every 5 s. **Placement trusts `slots_free`** (computed by
  `registry.FreeSlots`: tap/IP availability bounded by memory admission) — NOT
  `slots_total - slots_used`, which can overstate capacity when larger memory overrides are
  running; a host still building its golden snapshot advertises `slots_free=0` so fresh
  hosts aren't boot-stormed with cold creates.
  The gateway (`internal/gateway`) holds **no durable state**: it rebuilds
  its `sandbox_id → host` routing table from heartbeats, so it self-heals after a restart once
  each host reports. `POST /sandboxes` first consumes fleet-wide `warm_ready` capacity,
  bin-packing and reserving those ready VMs across hosts before it falls back to the fullest
  host with ordinary free slots. Snapshot adoption deliberately uses ordinary placement so
  it cannot steal default-create ready capacity. Both modes reserve at pick time so concurrent
  creates see each other. A create that a host rejects with a
  capacity-class error (503/429, e.g. pool exhaustion) or a connection failure **fails over**
  to the next-best host (≤3 attempts, the failing host penalized ~2 heartbeats), while genuine
  host errors return 502 without retry. When no slot is free the create
  waits in a bounded queue (`--queue-wait`/`--queue-max`, defaults 240s/4096; depth exported as
  `sandbox_create_queue_depth` and fed into the autoscaler signal) before 503ing with
  Retry-After. Id-scoped requests (incl. `/exec/stream` + `/shell`) are
  reverse-proxied to the owning host (one cached proxy per host over a shared tuned
  transport) with the host's token injected; `GET /sandboxes` scatter-gathers in parallel.
  Point the CLI at it with `--gateway <addr> --gateway-token <tok>`. The elastic fleet
  (Nomad autoscaler + GCE MIG) lives in `infra/gcp/` — `SLOTS_PER_HOST` in `config.env` is
  the source of truth for RUNNING capacity (taps/IPs/mem_budget_mib); `deploy-job.sh`
  generates those pools from it. Three knobs decouple the pools that used to all scale off
  `SLOTS_PER_HOST`: `PORTS_PER_HOST` (default 4× slots) sizes the port pool independently
  since hibernated sandboxes hold only their port; `GUEST_SUBNET_BITS` (default 24) widens
  the guest subnet past a single /24 so a host can run more than ~250 sandboxes at once;
  `MEM_PER_SLOT_MIB` (default 1180) sets committed memory per slot so a small-sandbox fleet
  can pack many more running sandboxes into the same host RAM — see the capacity-sizing
  notes under Architecture notes.
- **Creates are hot by default (golden snapshot).** On startup (`ensureGolden` in
  `internal/server/golden.go`) the server adopts or builds a **golden snapshot**: it
  cold-boots a throwaway pristine sandbox, snapshots it (marked `golden=1`, at most one via
  partial unique index), destroys the source, and keeps the snapshot's baked rootfs staged
  at `SourceRootfsPath` permanently (Firecracker opens that path during every LoadSnapshot).
  `POST /sandboxes` then clones it — the identity-neutral fan-out mechanism with N=1 — and
  **falls back to cold boot** on any failure (no golden yet, snapshot deleted, clone error),
  so clients see the same API either way. The golden snapshot records the base rootfs
  mtime+size; a rebuilt base (e.g. `install-agent`) invalidates it on the next server
  restart — restart `serve` after changing the base image. Opt out with
  `"disable_hot_create": true` in the config.
- **The golden can be BAKED onto a data-disk image so a fresh host adopts instead of
  building it** (fleet fast-scale; `infra/gcp/bake-image.sh golden`). `buildGolden` writes a
  self-describing manifest `golden.json` (the snapshot row + `base_mtime`/`base_size`, which
  are `json:"-"` on the row so the manifest carries them explicitly) into `SnapshotDir`. On
  startup, when the registry has no golden row (a fresh worker whose data disk was seeded from
  the golden image but whose SQLite is empty), `ensureGolden` calls `importGoldenManifest`:
  it reconstructs the row, re-validates via `goldenUsable` (artifacts on disk + base rootfs
  mtime/size match), `CreateSnapshot`s it, and falls into the normal adopt path. **Every
  failure mode — absent/corrupt manifest, stale artifacts, insert error — returns
  "not ok" and cold-builds**, so a bad or missing manifest is never worse than today. This
  removes the ~2 GB rootfs copy, the golden cold-build, AND the `slots_free=0` warming window
  from the scale-up path. It relies on the base rootfs mtime being STABLE at boot (goldenUsable
  keys on base rootfs mtime+size): the image bakes sandboxd into `/opt/fc` (`bake-image.sh
  [3b/6]`), so **`run.sh` does NOT run `install-agent`** — doing so re-bakes whenever the GCS
  release sandboxd differs from the baked one (two independent build paths differ by build-stamp
  bytes ALONE), bumping the mtime and forcing every host to cold-build the golden. **sandboxd is
  image-pinned: to ship a new agent, rebake (`./bake-image.sh bake && golden`) and roll.**
  `mig.sh` seeds each worker's data disk from `$GOLDEN_DATA_IMAGE_FAMILY` when it exists (blank
  disk + cold build otherwise). The `sandbox` SERVER binary is still pulled from the GCS release,
  so a host can only ADOPT if that release has `importGoldenManifest` (deploy ≥ this change).
  Rebake both images together (a drifted pair just cold-rebuilds).
- **Per-sandbox resource overrides cold-boot.** `POST /sandboxes` takes optional `vcpus` /
  `mem_mib` (0/absent = template default; bounds-checked in `validateResources`,
  `internal/server/server.go`). Firecracker bakes vcpus/mem into snapshots, so an override
  can't be served from the golden snapshot — it always takes the slower cold path.
  Restore/fanout bodies **reject** nonzero `vcpus`/`mem_mib` with 400 (a restored VM
  runs whatever its snapshot baked; snapshot rows record the source's values so restored/
  cloned rows report the truth). Hibernate/wake restores from snapshot, so overrides survive
  automatically. **API responses always report effective resources**: the registry keeps
  0 (= template default) but every sandbox-returning handler runs `effectiveResources`,
  filling in the template's vcpus/mem — so clients never see an absent value. `GET /info`
  exposes the template defaults + override limits (gateway forwards it to a live host).
- **The shell WebSocket is a supported client API, and it authenticates via the
  SUBPROTOCOL — never the query string.** Browsers can't set headers on a WebSocket,
  and `?access_token=` was removed in `6e4f1c0` (it leaks credentials into URLs, proxy
  traces and access logs; both bearerAuth middlewares reject query credentials and both
  proxies strip them). Clients instead offer two subprotocols:
  `sandbox.bearer.<base64url(token)>` plus the negotiable `sandbox.shell.v1`
  (`internal/wsutil`: `UpgradeAuthorization`, `StripBearerSubprotocol`,
  `EchoSubprotocol`). Three constraints make this work and are easy to break:
  **(1)** the token is base64url WITHOUT padding because a subprotocol name must be an
  RFC 7230 token — standard base64's `/` and `=` make browsers throw at construction,
  before any request is sent; **(2)** the server MUST echo a selected subprotocol, or a
  client that offered one fails the connection — since the guest agent doesn't negotiate,
  the hop that consumed the credential echoes `sandbox.shell.v1` via `ModifyResponse`
  (this is why clients offer a second, credential-free entry: so the secret is never
  reflected back); **(3)** subprotocol credentials are accepted on PUBLIC routes only —
  the internal-control check runs BEFORE the fallback, because the caller picks the
  upgrade headers, so "worker routes are never WebSockets" is not enforceable here.
  Errors on WS endpoints (bad token, unknown id, failed wake, agent unreachable) are
  delivered via `internal/wsutil.Reject`: complete the 101 handshake, then close with
  code 4000+HTTPstatus and the message as the close reason — a plain 401/404 would reach
  browsers as an opaque 1006. `Reject` must hijack via
  `http.NewResponseController(w).Hijack()`, NOT a `w.(http.Hijacker)` assertion:
  `httpapi.Middleware` wraps the writer in a `statusWriter` that embeds the
  ResponseWriter *interface* and exposes only `Unwrap`, so the direct assertion fails and
  every WS error silently degrades to the 1006 this whole mechanism exists to prevent
  (that regression shipped, and made the SDK's pty unusable against the fleet). The SDK's
  `sandbox.pty` maps 4401/4404 back onto AuthenticationError/NotFoundError.
- **Clone reidentify is signaled by gratuitous ARP.** A fan-out/hot-create clone resumes on
  an UNBRIDGED tap still carrying the snapshot's baked IP; the in-guest thaw agent adopts the
  fresh identity from MMDS then broadcasts GARPs (`cmd/sandboxd/garp_linux.go`). The host
  opens `provisioner.ListenARP` on the tap **before resume** and `finishClone` bridges the
  moment the announce arrives (~200-400ms); timeout after 1.5 s falls back to bridging anyway
  (matches snapshots whose baked agent predates the announce). New sandboxd must be baked via
  `install-agent` for the fast path.
- **Guest wall clock is stepped on every snapshot resume.** Firecracker restore leaves the
  guest's CLOCK_REALTIME frozen at snapshot-creation time (hours stale for golden-snapshot
  hot creates on a long-lived server), and NTP is NOT a fallback (some deployments block
  outbound UDP). Two host→guest signals cover all four resume paths (hot create, fan-out,
  1:1 restore, hibernation wake — both same-identity and clone-path): `epoch_ms` in MMDS
  (StartClone identity doc; `vm.PushEpoch` on restore/wake), which the thaw agent polls on
  a 200ms tick, plus a deterministic `POST /clock` (`agentapi.ClockSyncRequest` →
  clock_settime in the guest) fired by `syncGuestClock` right after each path's readiness
  gate, so a sandbox is never handed out with a stale clock. Old baked agents 404 the
  /clock call — logged, never fatal. Re-run `install-agent` to bake the new sandboxd.
- **UFFD lazy page-in on wake (opt-in).** With `"uffd_restore": true`, the same-identity
  hibernation wake (`wakeRestore`) restores via Firecracker's userfaultfd memory backend
  instead of the eager File backend: `vm.RestoreUFFD` issues `PUT /snapshot/load` over the
  raw socket with `mem_backend={backend_type:"Uffd", backend_path:<sock>}` (SDK v1.0.0 has
  no `mem_backend` field, so this reuses the clone path's raw `fcAPI`, not `WithSnapshot`)
  and resumes before RAM is paged in; the guest faults its working set from the mem file
  on demand. The handler (`internal/vm/uffd_linux.go`) receives the uffd over SCM_RIGHTS,
  mmaps the (already-materialized, full) mem file read-only, and services each fault with
  `UFFDIO_COPY`. Wake latency/I-O then track the working set, not guest size. The handler
  is host-local (one page-fault goroutine + OS thread per awake UFFD VM); its mem mapping
  is unmapped by that goroutine only after Firecracker exits (never by `close()`, which
  just drops the socket) so a page copy can't race the unmap. **Only same-identity wake is
  UFFD-backed; the clone-path wake still uses File.** **Default off, and fleet measurement
  (2026-07-20) says keep it off for the current small-guest workload**: File-backend wake is
  already ~80 ms warm (mem file is small + page-cache-warm, so the "eager" load just maps
  cached pages), while UFFD's per-4 KiB-fault userspace round-trip adds ~30–50 ms. UFFD only
  wins when eager load is expensive — large guests, cold/uncached mem files, or remote/GCS
  memory (scale-to-zero Model B). NB `page_size_kib` in FC v1.15's UFFD message is actually
  BYTES (4096), not KiB — `pageSizeBytes()` normalizes it; getting this wrong made 4 MiB
  "pages" and an offset that panicked. The fault loop has a `recover()` so a handler bug
  degrades to a failed wake, never a serve crash. See docs/scale-to-zero.md.
- **`/etc/resolv.conf` in the guest must be a REAL FILE, never a symlink to
  `/proc/net/pnp`.** The config's nameservers reach the guest only through the kernel
  `ip=` boot param, which the kernel re-exposes at `/proc/net/pnp` in resolv.conf
  format, so symlinking the two looks like the clean way to honor the host config
  without baking it in. It isn't: `/proc` files report `st_size=0`, and any resolver
  that sizes a file before reading it sees an EMPTY config. c-ares — which backs
  Node's `dns.resolve*`/undici, and therefore Claude Code's "Checking
  connectivity..." probe — is one of those; finding no nameservers it falls back to
  `127.0.0.1:53`, where nothing listens. The symptom is DNS that works for
  curl/git/npm/python (glibc reads the symlink fine) and fails for anything
  c-ares-based: an "unstable internet" that depends on which tool you reach for,
  and a `claude` that dies with `Failed to connect to api.anthropic.com: ETIMEDOUT`
  ~30 s in while `curl` to that same host takes 50 ms. So the pnp content is COPIED
  into a regular file, in two places: `sandbox-resolvconf.service` at boot
  (build-devbox-rootfs.sh) and `materializeResolvConf` on sandboxd startup
  (cmd/sandboxd/resolvconf.go) — the latter is what repairs an older rootfs once a
  new agent is baked in. Snapshot-restored guests (hot create, fan-out, wake) resume
  a live process and re-run neither, but inherit the file through the rootfs.
- **Guest MTU is 1500 and the host fabric may be smaller, so the host clamps MSS.**
  Firecracker's virtio-net hands the guest a fixed 1500-byte MTU with no way to pass
  the host's through, so on GCP (VPC MTU 1460) every guest advertises an MSS 40 bytes
  too large. It still works — the host drops the oversized frame and returns ICMP
  frag-needed, PMTU discovery recovers — but it costs a drop + retransmit per new
  connection per destination (measured ~2.4 retransmits/connection, and the PMTU
  cache expires in ~10 min), and it hard-stalls wherever that ICMP is lost or
  rate-limited. `EnsureNetwork` therefore adds a `-t mangle FORWARD ... TCPMSS
  --clamp-mss-to-pmtu` rule; it's adaptive (a no-op on a 1500-MTU host like Hetzner)
  and **best-effort**, since it needs `xt_TCPMSS` and a host missing that module
  should still serve. The guest also sets `tcp_mtu_probing=1` as a black-hole
  backstop. Don't "fix" the MTU by setting the tap/bridge instead — virtio-net won't
  propagate it to the guest.
- **SSH into a sandbox rides the existing port proxy.** The base rootfs bakes
  `openssh-server` (key-only login as the unprivileged **`sandbox` user**, uid
  1000: `PermitRootLogin no`, `PasswordAuthentication no`, `AllowUsers sandbox`,
  in `sshd_config.d/sandbox.conf`), and `ssh.service` is enabled (socket
  activation disabled) so :22 listens the instant the guest boots. There is no
  root login — `root@` is refused by sshd, not merely unauthorized.
  **Host keys are unique per sandbox and never baked into the image**: the base
  rootfs ships with none, and sandboxd's `POST /identity`
  (`initializeGuestIdentity`, cmd/sandboxd/identity.go) removes any inherited
  ones and generates a fresh key on every independent create — so no two
  sandboxes, golden clones included, can impersonate each other. **Ed25519
  only** (~7 ms), and `sandbox.conf` pins `HostKey
  /etc/ssh/ssh_host_ed25519_key` so sshd doesn't warn about the absent
  RSA/ECDSA keys: `ssh-keygen -A` also built RSA-3072, which cost ~1.2 s in a
  2-vCPU guest and was essentially the entire `/identity` call. That config is
  written by both `build-devbox-rootfs.sh` and `install-agent` (the latter
  repairs an older base image) — keep the two in sync. `POST /sandboxes` takes
  an optional `ssh_pubkey` (one OpenSSH key line, `validateSSHPubkey` in
  server.go — rejects multi-line/unknown-type); after the create readiness gate
  (both cold and hot paths), `installSSHKey` (proxy.go) posts it to sandboxd's
  `POST /ssh-key`, which writes `/home/sandbox/.ssh/authorized_keys` — under
  `withGuestFilesystem`, so the dir and file land owned by `sandbox:sandbox`
  (0700/0600) as sshd demands. It is NOT best-effort like `syncGuestClock`: a
  key-install failure destroys the sandbox and fails the create, so a box handed
  back with SSH requested is always reachable. The key lives in the rootfs, so
  it survives hibernation/wake with no re-push. Reach it by exposing guest :22
  as a host port — the userspace TCP proxy forwards it with wake-on-connect, so
  an incoming SSH connection wakes a hibernated sandbox and pins it for the
  session, exactly like a forwarded HTTP port. Old baked sandboxd 404s
  `/ssh-key` (re-run `install-agent`; rebuild the base for openssh first).
  CLI: `sandbox up --ssh-key ~/.ssh/id_ed25519.pub` (file path or key literal),
  then `sandbox ssh <id>` (forwards :22 on first use) or
  `sandbox ssh-config <id>` to drive plain ssh/scp/rsync/editors. Both take
  `--jump <bastion>`.
- **Current jailed production benchmark result (2026-07-29, release `9b6a9fc`;
  pool hardening landed in `f12c004`):** the production pool is **8 ready VMs per active worker**
  (`warm_pool_size: 8`). A 16-way hold burst across two active workers was
  **16/16 ready-pool hits**, zero misses/errors, and zero pool-build failures.
  Measured from the control VM (removing the Greece→India SSH/Tailscale path),
  two runs were p50 **71/82 ms**, p95 **130/118 ms**, max **130/118 ms**.
  A create that also installed a fresh Ed25519 client public key completed in
  **92 ms**. Repeated 16-way workloads from Greece were p50 **414–426 ms**,
  p95 **589–600 ms**: all creates were still confirmed ready hits, so that tail
  is transport RTT/tunneling rather than VM creation. A fresh 25-iteration
  production lifecycle run on `9b6a9fc` measured create p50 **200 ms**, p95
  **304 ms**, max **404 ms**. The earlier run was p50 **198 ms**, p95 **204
  ms** (direct worker p50 **196 ms**, p95 **201 ms**). Historical results are in
  `sdk/typescript/benchmarks/results/lifecycle_c990555_gateway_20260729.json`
  and `lifecycle_c990555_20260729.json` (gitignored). The latest extensive
  campaign is in `production_extensive_9b6a9fc_20260729/`: 22/25 direct
  default-source creates were ready-pool hits in **8–15 ms**, while three
  refill-bound creates took **1.756–2.132 s**; snapshot-source create p50 was
  **1.897 s**. Fleet default passed at **32/32** and **64/64**, but the 128-way
  run created **80/128** because one placement-eligible worker's jailer
  `io.max` referenced a block device absent on that host. The 64-way fsync run
  created 64/64 but one workload hit SQLite `database is locked`; the 64-way
  large run passed 64/64. Snapshot source saved **452 MiB PSS (15.5%)** across
  32 VMs versus default source. All created resources were verified deleted and
  the final gateway sandbox count was zero. Treat sub-500 ms as a
  ready-capacity objective, not an unconditional create guarantee; validate
  jailer I/O devices before a worker becomes placement-eligible. Root cause on
  the failing worker: `/etc/fstab` had accumulated stale data-disk UUID entries,
  `/dev/sdb` was XFS but unmounted, and `/mnt/sandbox-data` silently fell back
  to boot-disk `/dev/sda1`. `startup-worker.sh` ignored the resulting
  `xfs_growfs` failure, stamped `data_disk_ready`, and started Nomad anyway.
  **Post-benchmark fix:** `startup-worker.sh` now replaces every existing
  fstab row for the mountpoint, mounts the named disk explicitly, validates the
  backing major:minor, XFS type, and read-write options before and after a
  non-optional `xfs_growfs`, and keeps Nomad disabled/stopped until those
  checks pass. `infra/gcp/startup-worker_test.sh` covers stale rows, the exact
  boot-disk fallback, wrong filesystem/options, and the Nomad admission gate;
  `make validate-infra` runs it. The fix was deployed in instance template
  `sandbox-workers-tpl-20260729-093155`: both active workers reported XFS
  `8:16` read-write and startup exit 0, all six standby workers registered with
  Nomad before suspending, and the restored MIG reached 2 running + 6
  suspended on the new template. A production create/delete smoke completed
  create-to-ready in **30 ms**, returned delete 204 then get 404, and left zero
  sandboxes. Release `9b6a9fc` predates the startup-script fix; repeat the
  128-way benchmark before declaring the measured fleet result healthy.
  A ready row is a normal jailed Firecracker VM with its own UID/GID, cgroup
  leaf, PID namespace, seccomp policy, tap/IP, rootfs, guest network identity,
  clock, and freshly rotated Ed25519 SSH host key. It consumes normal slot and
  memory capacity but is excluded from routes/lists; create atomically promotes
  it to `running`, resets `created_at` to claim time, applies request fields/key,
  and replenishes the pool concurrently in the background. A build remains
  `preparing` and unclaimable until every launch/readiness/security gate has
  completed; only then does `MarkWarmReady` promote it. The maintainer polls as
  well as accepting kicks, so an unexpectedly dead ready VM is replenished.
  Pool startup waits through the standby placement-delay window so refill VMs
  can suspend without nested-VM interference. `sandbox_warming`,
  `sandbox_warm_preparing`, `sandbox_warm_claims_total`,
  `sandbox_warm_misses_total`, and `sandbox_warm_build_failures_total` expose
  this lifecycle; the gateway also exports aggregate/per-host `warm_ready`.
  **Corrected attribution:** phase timing on an exhausted pool measured a hot
  jailed launch at roughly **24–47 ms** (`prepare` + process-to-API; a cold
  first staging pass can be ~123 ms), not the previously inferred ~390 ms.
  The resumed guest's network re-identification was **333–399 ms** and SSH
  identity readiness **125–136 ms**; a private tap wake hint did not remove
  that guest-resume floor. An exhausted ready pool therefore still falls back
  to an ordinary secure clone (~734 ms end-to-end in the production probe).
  Size the pool for the latency-critical arrival burst; do not weaken or bypass
  the jailer/network/identity gates to make the fallback look faster.
- **known_hosts is keyed on the sandbox ID, not host:port**
  (`cmd/sandbox/ssh.go`). Host keys are unique per sandbox but host ports are
  recycled from a pool, so OpenSSH's default identity — `[host]:port` — makes
  the *second* sandbox handed port N look like a compromise of the first:
  `REMOTE HOST IDENTIFICATION HAS CHANGED!` and a refused connection. The fix is
  `HostKeyAlias=sandbox-<id>` (a UUID, never reused), which both `sandbox ssh`
  and the generated stanza set, plus `CheckHostIP=no` (when on, OpenSSH *also*
  stores an address-keyed entry and the collision returns; it only defaults off
  since OpenSSH 8.5) and `StrictHostKeyChecking=accept-new` (a fresh alias has
  no stored key to compare against, so the prompt is pure friction, while a
  changed key for an alias already known still fails). Don't "fix" this with
  `StrictHostKeyChecking=no` + `UserKnownHostsFile=/dev/null` — that disables
  the check instead of scoping it, and it's why `tests/security-gate.sh` needs
  those flags. `sshOptions` is the single source for the set; a test asserts the
  wrapper and the stanza can't drift. **Fleet caveat:** the gateway is an HTTP
  reverse-proxy and does NOT forward raw TCP, so fleet SSH needs a ProxyJump to
  the owning worker (`--jump`; the sandbox's `host_addr` names that worker) or a
  WS tunnel — the gateway itself will never route it.
- **Guest agent readiness gates create.** `handleCreate` polls `http://guestIP:8090/health`
  for up to 60 s and tears the sandbox down if the agent never answers. If the base rootfs
  lacks sandboxd (fresh build, forgot `install-agent`), every create will fail this way —
  that's the first thing to check.
- **Memory is admission-checked; CPU is deliberately oversubscribed (~6:1).**
  `mem_budget_mib` in the config (deploy-job.sh injects `SLOTS×1180`; 0 = derive host
  total − 2 GiB; <0 = off) caps the SUM of committed guest memory — each running
  sandbox's effective `mem_mib` + 156 MiB VMM overhead; hibernated VMs hold none. The
  check runs inside the registry TX of `Create`/`CreateRestore`/**`Wake`** (waking
  re-commits the snapshot's baked memory; a rejected wake rolls back to hibernated and
  surfaces as 503 on agent-bound requests / close code 4503 on the shell WS), returns
  `ErrMemExhausted` (wraps `ErrPoolExhausted` so 503 + gateway failover fire unchanged),
  and bounds `FreeSlots` — a big-mem sandbox eats multiple slots' worth of `slots_free`,
  so placement and autoscaling see the truth and the Nomad cgroup can never be
  OOM-blown by `mem_mib` overrides. `maxMemMIB` (the per-sandbox override ceiling and
  `GET /info` MaxMemMIB) is clamped to the budget. vcpus have NO sum guard by design:
  the Nomad task runs CPU *shares*, so contention degrades to fair-share slowdown —
  there is no CPU analogue of the OOM killer.
- **Creates are bounded and capacity-classed.** A per-host semaphore
  (`"create_concurrency"` in the config; 0 = min(2×NumCPU, 16)) gates every bring-up
  (hot clone, cold boot, 1:1 restore) so a burst queues in-process instead of
  boot-storming the host into agent timeouts — the 60 s agent gate starts ticking only
  after acquisition. Pool exhaustion (`registry.ErrPoolExhausted` from the tap/IP/port
  pickers) returns **503 + Retry-After**, not 500, so the gateway/SDK can tell capacity
  from failure; `client.APIError` carries the status code through `internal/client`.
- **exec kills whole process groups.** sandboxd runs commands with `Setpgid` and kills
  `-pgid` on timeout so shell children don't outlive the request. stdout/stderr are capped
  at 2 MiB each (`agentapi.MaxOutputBytes`).
- **Streaming exec is NDJSON, not SSE.** `POST .../exec/stream` emits
  `agentapi.ExecEvent` lines (stdout/stderr/exit); the server proxy wraps the
  ResponseWriter in a flush-on-write writer so chunks pass through immediately. All
  non-Type ExecEvent fields are omitempty — decoders must treat absent fields as zero.
- **Interactive shell is a WebSocket PTY.** `GET /sandboxes/{id}/shell` upgrades and
  `handleShellProxy` reverse-proxies it to the guest's `/shell` via `httputil.ReverseProxy`
  (Go handles the Upgrade handshake + raw byte copy natively, so the host needs no
  WebSocket lib and it works over both the Unix socket and the TCP listener). In the guest,
  sandboxd runs `bash -l` on a real pty (`creack/pty`): binary frames are raw terminal bytes
  both ways, text frames are JSON `agentapi.ShellControl` resizes. Clean exit closes the
  socket with reason `exit:<code>`; client disconnect kills the shell's process group. See
  the protocol doc-comment in `agentapi`.
- **TTL reaper.** `POST /sandboxes` accepts optional `{"timeout_sec":N}`; a 10 s ticker
  goroutine in `Serve` destroys rows whose `expires_at` passed (running AND hibernated).
  `POST .../timeout` resets (0 clears). No default TTL — absent means live forever.
- **Server shutdown hibernates, never destroys.** `shutdownAll` freezes every
  running sandbox (bounded-parallel, 100 s budget, `force` past activity pins;
  fallback destroy per sandbox on failure). This is what makes MIG standby-pool
  stop/start cycles and autoscaler scale-in non-destructive: the frozen rows +
  artifacts live on the persistent disk, and the next `serve` start re-binds
  their port listeners and heartbeats their ids. Requires `vmCtx` to be
  DECOUPLED from the serve ctx (a cancelled serve ctx makes the firecracker
  SDK / clone CommandContext kill VMs before anything can be frozen).
- **Diff hibernation + the diffBase map.** Hibernate (and user snapshots)
  write a DIFF against the golden base only while `Server.diffBase` has an
  entry for the machine: set when a clone is loaded from a snapshot, deleted
  after ANY snapshot attempt (Firecracker resets the dirty bitmap at snapshot
  creation) and never set for hibernation-woken machines (their bitmap tracks
  the hib artifacts, not the golden). Do NOT gate diffs on `sb.BaseSnapshotID`
  — it is never cleared, and trusting it silently corrupts memory on restore.
  A diff freeze writes a `diff_base` marker next to the mem file; wake rebases
  via `materializeHibMem` (reflink + sparse overlay; GCS base pull fallback).
- **Idle hibernation** (`internal/server/hibernate.go`; `"hibernate_after_sec"` in the
  config sets the host default, 0 = off; `POST /sandboxes` accepts a per-sandbox
  `hibernate_after_sec` override — >0 custom window, -1 never, 0 inherit — also on
  restore/fanout bodies and SDK `hibernateAfterMs`). Sandboxes idle past their window
  are paused + full-snapshotted
  (mem/state under `snapshots/hib-<id>`; the rootfs file just stays put), the VM killed,
  and the row flipped to `status=hibernated` — releasing tap/IP back to the pools
  (their partial unique indexes only bind `running`), so hibernated sandboxes hold no
  slot and survive server restarts (reconcile skips them). Host ports are the exception:
  they stay hard-reserved (`uniq_port_held` covers hibernated rows, `loadUsed` counts
  them as used) because the port-proxy listeners stay bound across the freeze. Any
  agent-bound request (exec/files/dir/shell) wakes transparently via `ensureRunning`:
  same-identity plain restore when the old tap+IP are free — the common case, because
  the pool pickers soft-avoid hibernated taps/IPs — else the fan-out clone path (fresh
  identity, MMDS reidentify with a fresh Gen, GARP). Manual trigger: `POST .../hibernate`
  / `sandbox hibernate <id>`. Activity = API traffic AND forwarded-port traffic:
  in-flight requests (open shells, exec streams) and open forwarded-port connections
  pin the sandbox running, and **a connection to a forwarded host port wakes a
  hibernated sandbox** (the userspace proxy wakes via `ensureRunning`, then dials the
  guest's current IP). Heartbeats report hibernated ids for routing but exclude them
  from `slots_used`.
- **Port forwarding is a userspace TCP proxy, not DNAT**
  (`internal/server/portproxy.go`). The server binds every explicitly mapped
  host port (`sandbox_ports` rows) with an in-process listener: accept → record
  activity + pin (same `act.begin` mechanism as API requests) → `ensureRunning` (wakes
  if hibernated) → re-read the row for the CURRENT guest IP (a clone-path wake changes
  it — never cache it) → dial guest → bidirectional copy with TCP half-close. Listeners
  open on expose, persist through hibernation (that's what makes
  wake-on-connect work), re-bind at startup for hibernated rows (`reopenPortListeners`),
  and close on destroy. `RemovePortForward*` (iptables `-D`) is kept and still called in
  destroy/reconcile purely as legacy cleanup for hosts upgrading from the DNAT scheme.
- **Port mappings** live in the `sandbox_ports` table and draw host ports from the
  configured port pool. destroy() and reconcile()
  must close their listeners (and remove legacy DNAT rules) — read mappings before
  deleting rows. `exposePort` works on a hibernated sandbox without waking it: the new
  listener is just another wake-on-connect entry point.
- **`vmCtx` ≠ request ctx.** `handleCreate` must pass `s.vmCtx` (server-scoped) to `vm.NewMachine`
  and `vm.Start`, NOT `r.Context()` — the request ctx cancels when the handler returns, and the
  firecracker SDK SIGTERMs the VM when its ctx cancels. This was an early bug that wasted hours.
- **Pools allocated atomically via SQLite.** `registry.Create` runs INSERT inside a TX with
  partial unique indexes (`uniq_tap_running`, `uniq_ip_running`) guaranteeing no two
  running sandboxes share a tap/IP; `sandbox_ports.host_port` is independently unique. Concurrent
  creates that race lose to UNIQUE constraint and surface as 500.
- **The port pool is sized independently of tap/IP/memory, because hibernation doesn't
  release it.** Taps/IPs/`mem_budget_mib` bound concurrently *running* sandboxes (real
  compute capacity); a hibernated sandbox holds only its port (taps/IPs free on hibernate),
  so the port pool is really the ceiling on *total* sandboxes (running + hibernated) per
  host. `deploy-job.sh` generates `PortMax` from `PORTS_PER_HOST` (defaults to 4×
  `SLOTS_PER_HOST` if unset, matching the fleet's original fixed ratio) rather than tying it
  to `SLOTS_PER_HOST` directly — raise it independently when sandboxes run much smaller than
  `MEM_PER_SLOT_MIB` and you want more hibernated at once than the default ratio allows.
- **The guest subnet width is configurable (`guest_subnet_bits`), and it — not a hard-coded
  /24 — is the ceiling on concurrently RUNNING sandboxes per host.** Every running sandbox
  needs a guest IP; a /24 holds ~253, /22 ~1021, /20 ~4093. The prefix is applied at three
  sites that MUST agree or guests can't route to the gateway: the bridge/gateway CIDR
  (`cmd/sandbox/serve.go`), the cold-boot guest CIDR (`server.go` handleCreate), and the
  clone-path MMDS reidentify prefix (`CloneParams.Prefix` in `snapshot.go` fan-out +
  `hibernate.go` wake — the in-guest thaw agent flushes eth0 and re-adds `ip/prefix`, so
  hot-created clones adopt the configured width even if the golden was baked at a different
  one). Widen it via `GUEST_SUBNET_BITS` in `config.env`; `deploy-job.sh` then spans the
  guest-IP pool across octets (proper 32-bit IP arithmetic, no longer last-octet-only) and
  refuses a `SLOTS_PER_HOST` that would overrun the subnet's usable range or hit its
  broadcast address. Default 24 keeps every existing config byte-identical.
- **Committed memory per slot is a knob (`MEM_PER_SLOT_MIB`, default 1180), decoupled from
  the 1 GiB template assumption.** `mem_budget_mib` (admission ceiling) and the Nomad task
  cgroup (`TASK_MEMORY`) both derive as `SLOTS_PER_HOST × MEM_PER_SLOT_MIB` (+2 GiB for
  serve). A small-sandbox fleet (e.g. 128 MiB guests) lowers it to ~300 so the same host RAM
  admits many more running sandboxes; the memory-admission check in `registry` still sums
  each sandbox's *actual* effective `mem_mib` + overhead, so this only sizes the budget, not
  the per-sandbox charge.
- **Per-VM rootfs is a CoW clone where the filesystem allows it.** `provisioner.CloneFile`
  (used by `PrepareRootfs` for cold boot, and by `CloneRootfs`/`CopyFileSparse` for
  restore/fan-out/hibernate) tries `cp --reflink=always` first — instant, near-zero disk
  on XFS/btrfs (the GCP data disk is formatted XFS specifically for this) — and falls back
  to a full `cp --sparse=always` only when the filesystem can't reflink (e.g. ext4, where
  it's ~2 GB-sparse copy in ~1 s and I/O scales linearly with N). Don't share the rootfs
  between VMs — ext4 corrupts under concurrent mount.
- **The worker boot/readiness path is instrumented per stage** (`internal/server/bootphase.go`).
  Autoscale latency is ~10 s of control loop plus a much larger "make this host usable" span
  that used to be one opaque block in the profile. Three writers that can't share memory
  contribute phases: `startup-worker.sh` and the Nomad task's `run.sh` append
  `"<phase>\t<epoch_ms>"` to **`/run/sandbox/boot-phases`** (fixed path — the startup script
  runs before any config is read; `SANDBOX_BOOT_PHASES` overrides for tests), serve marks its
  own (`serve_process_start`, `reconcile_done`, `golden_settled`, `first_heartbeat_ok`,
  `capacity_advertised`), and `kernel_boot` comes from `/proc/stat` btime as a free stand-in
  for "GCE reported RUNNING". `/metrics` exports `sandbox_boot_phase_timestamp_seconds{phase}`,
  `sandbox_boot_phase_seconds{phase}` (offset from the anchor) and the headline
  `sandbox_worker_ready_seconds`; all federate through `/metrics/hosts` with a `host` label
  automatically (`injectHostLabel` merges into existing labels). **These are absolute
  timestamps, not rates — so the normal 10 s scrape recovers ms-accurate boundaries and
  profiling needs no special scrape interval.** Marks are first-write-wins (the 5 s heartbeat
  re-marks forever). `capacity_advertised`, not `first_heartbeat_ok`, is the real "new capacity
  online" moment: an unwarmed host deliberately heartbeats `slots_free=0`. Note `/run` is
  tmpfs, so a *stopped* standby worker gets a clean timeline on boot while a *suspended* one
  keeps its original boot's file — correct, since a resumed worker re-runs neither the startup
  script nor serve. NB `parseMetrics` in the server tests skips float-valued lines for these
  families; it still fails on genuinely malformed output.
- **The autoscaler's scale-out confirmation budget is a scale-up BLACKOUT, so it's tuned
  SHORT.** The gce-mig target polls for MIG-wide stability after a resize, and while that runs
  the policy sits in `StateScaling` where every evaluation is dropped ("skipping scaling,
  target still scaling"). The upstream default of 15 attempts × 10 s meant **150 s during which
  a growing burst could not add a second wave of hosts**, and it always ended in
  `failed to confirm scale out GCE Instance Group: reached retry limit` — because with a
  standby pool, stability is unreachable by construction: the MIG keeps replenishing suspended
  workers in the background (~190 s). We don't need GCE's confirmation, since real readiness
  arrives on the gateway heartbeat (now measured by `sandbox_worker_ready_seconds`). So
  `retry_attempts` is set from `AUTOSCALER_RETRY_ATTEMPTS` (default 3).
  **This does NOT shorten the blackout to 30 s — that earlier claim was wrong and was
  disproved on 2026-07-25.** `retry_attempts=3` only makes the
  `failed to confirm scale out ... reached retry limit` ERROR appear sooner (~21 s, 3 attempts
  × ~7 s gRPC timeout); the policy is not released then. Timing consecutive
  `calculating scaling target` lines measured **188.87 s and 188.94 s** in a single 160-burst —
  a deterministic ~189 s blackout, matching the background standby-replenish window, and not
  explained by `cooldown = "1m"`. **Always re-verify a blackout claim by the gap between
  consecutive `calculating scaling target` lines, never by the error timestamp.** The practical
  consequence: a burst gets exactly ONE scaling action, then ~3 min of nothing; and because the
  check uses `max_over_time(...[15m])`, the post-blackout evaluation replays the stale peak and
  over-scales *after* demand is gone. Failing fast is still safe: on confirm failure the handler
  returns to Idle **without** entering
  cooldown, and the next evaluation compares desired against the MIG target size the resize
  already set, so it no-ops unless demand genuinely grew. Requires **autoscaler ≥ 0.4.8**
  (older builds silently ignore the key and keep 150 s) — hence the bump to 0.5.0, which also
  brings 0.4.9's "don't issue scaling requests when no change is needed" and 0.5.0's scale-in
  node-selection fix. `control-install.sh` now compares the installed binary's version and
  re-fetches on mismatch; it previously guarded with `command -v nomad-autoscaler ||`, so
  **bumping `AUTOSCALER_VERSION` never actually upgraded anything**.
- **Build tags**: `//go:build linux` for SDK code, `//go:build !linux` for the stub. Keep the
  signatures identical in both files.
- **`disableValidation` arg on `NewMachine`** lets you build the SDK config on non-Linux for
  dry runs. Server passes `false`.
- **Firecracker stderr/stdout is captured** to `firecracker-<vmid>.log` in the server's cwd.
  After `/logger` is bootstrapped, firecracker writes most logs to its log FIFO (drained by
  the SDK, never persisted). For deep-dive debugging, switch `LogFifo` to a regular file path.

## Conventions

- Config merging: JSON file < CLI flags. Only `--config` and `--socket` flags exist now;
  per-VM overrides in `POST /sandboxes` are limited to `name`, `timeout_sec`,
  `hibernate_after_sec`, `vcpus`, `mem_mib`, and `ssh_pubkey`.
- Socket paths auto-generate UUIDs when left empty.
- Use `signal.NotifyContext` for signal handling, not raw `signal.Notify` + channel.
- Commits: short imperative subject lines (see `git log`). No co-author trailer.
  Land on `main` directly; don't open PRs or auto-branch.
- Use `modernc.org/sqlite` (pure-Go) NOT `github.com/mattn/go-sqlite3` — we need
  `CGO_ENABLED=0` to cross-compile from macOS.
- **Releasing the TS SDK** (from `sdk/typescript`) is five things, and the repo
  version being bumped does NOT mean a release happened — 1.0.0 sat bumped and
  unreleased for five days while consumers installed 0.4.0. Bump `version` in
  `package.json`, add a `CHANGELOG.md` entry, repoint the **pinned install URL**
  in `README.md` (it names an exact tarball, so a stale one keeps serving the old
  release), then `npm pack` (builds via `prepack`) and
  `gh release create sdk-v<version> sandbox-<version>.tgz`. Verify with
  `npm run typecheck`, `npm test`, and `npm run check:api` (regenerates
  `src/generated/api-v1.ts` from `api/openapi.yaml` and fails on drift). The
  install path is a GitHub Releases tarball, not a registry — there are no semver
  ranges, so "upgrading" means handing users a new URL.

## Not done yet

- **Only vcpus/mem are overridable on `POST /sandboxes`.** Kernel image, kernel args,
  rootfs, etc. remain template-wide. The body carries `name`, `timeout_sec`,
  `hibernate_after_sec`, `vcpus`, `mem_mib`, and `ssh_pubkey`.
- **No memory overcommit.** Guest memory is provisioned 1:1 (admission-enforced via
  `mem_budget_mib` — see the memory-admission note above). Hot-created clones share the
  golden snapshot's page cache and idle guests touch a fraction of their RAM, so real
  density headroom exists — but without a virtio-balloon/free-page-reporting device,
  dirtied pages never return until hibernation. Add a balloon before any overcommit knob.
- **Few tests on the Go side.** `internal/gateway` (placement, queue, metrics),
  `internal/registry` (hibernate/wake state machine, hibernated-port pinning, resource
  persistence), and `internal/server` (port proxy: forwarding, wake-on-connect, activity
  pinning; resource-override validation) have unit tests; the rest is covered by the
  TS SDK mock-server suite + the fleet e2e suite in `tests/`.
- **No TLS on the TCP listener.** `serve --listen <tailnet-ip>:8080 --token <tok>` exposes
  the API over TCP with bearer auth (constant-time compare); we rely on Tailscale for
  transport security. Don't bind it to a public interface. The Unix socket stays auth-free
  (mode 0600). The local token for the dev machine lives in `.sandbox-token` (gitignored).
