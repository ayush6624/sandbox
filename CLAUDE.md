# CLAUDE.md

## Project overview

Firecracker-based microVM sandboxes for development, exposed via a
local HTTP API over a Unix socket. Each sandbox boots Ubuntu 24.04 with Node 22,
pnpm, TypeScript, Python 3, and common build tooling (build-essential, git). It's
a bare sandbox — no app server runs on boot; the primary guest port (3000) is
forwarded to a host port for whatever you start there. e2b style — but
self-hosted, on bare metal.

Multi-sandbox: each one gets its own tap, IP, host port, and rootfs copy.
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
  sandbox_ids}` to the gateway every 5 s. **Placement trusts `slots_free`** (computed by
  `registry.FreeSlots`: min per-pool availability, counting hibernated sandboxes' held ports
  and extra exposed ports) — NOT `slots_total - slots_used`, which overstates capacity
  whenever hibernated port-holds bind the port pool; a host still building its golden
  snapshot advertises `slots_free=0` so fresh hosts aren't boot-stormed with cold creates.
  The gateway (`internal/gateway`) holds **no durable state**: it rebuilds
  its `sandbox_id → host` routing table from heartbeats, so it self-heals after a restart once
  each host reports. `POST /sandboxes` bin-packs onto the fullest live host with free slots
  (reserve-at-pick so concurrent creates see each other); a create that a host rejects with a
  capacity-class error (503/429, e.g. pool exhaustion) or a connection failure **fails over**
  to the next-best host (≤3 attempts, the failing host penalized ~2 heartbeats), while genuine
  host errors return 502 without retry. When no slot is free the create
  waits in a bounded queue (`--queue-wait`/`--queue-max`, defaults 180s/512; depth exported as
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
  from the scale-up path. It relies on the base rootfs mtime being STABLE across the image →
  data-disk copy: the image bakes sandboxd into `/opt/fc` (`bake-image.sh [3b/6]`) and
  `startup-worker.sh` stages with `--preserve=timestamps` + the `.agent-stamp` sidecar, so the
  boot-time `install-agent` is a no-op (short-circuits before the mount that would bump mtime).
  `mig.sh` seeds each worker's data disk from `$GOLDEN_DATA_IMAGE_FAMILY` when it exists (blank
  disk + cold build otherwise). Rebake both images together (a drifted pair just cold-rebuilds).
- **Per-sandbox resource overrides cold-boot.** `POST /sandboxes` takes optional `vcpus` /
  `mem_mib` (0/absent = template default; bounds-checked in `validateResources`,
  `internal/server/server.go`). Firecracker bakes vcpus/mem into snapshots, so an override
  can't be served from the golden snapshot — it always takes the cold path (~2 s vs ~250 ms
  hot). Restore/fanout bodies **reject** nonzero `vcpus`/`mem_mib` with 400 (a restored VM
  runs whatever its snapshot baked; snapshot rows record the source's values so restored/
  cloned rows report the truth). Hibernate/wake restores from snapshot, so overrides survive
  automatically. **API responses always report effective resources**: the registry keeps
  0 (= template default) but every sandbox-returning handler runs `effectiveResources`,
  filling in the template's vcpus/mem — so clients never see an absent value. `GET /info`
  exposes the template defaults + override limits (gateway forwards it to a live host).
- **The shell WebSocket is a supported client API.** Browsers can't set headers on a
  WebSocket, so upgrade requests may auth via `?access_token=` (accepted by both bearerAuth
  middlewares, upgrade requests only; the shell proxy strips it before the guest). Errors on
  WS endpoints (bad token, unknown id, failed wake, agent unreachable) are delivered via
  `internal/wsutil.Reject`: complete the 101 handshake, then close with code 4000+HTTPstatus
  and the message as the close reason — a plain 401/404 would reach browsers as an opaque
  1006. The SDK's `sandbox.pty` maps 4401/4404 back onto AuthenticationError/NotFoundError.
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
- **SSH into a sandbox rides the existing port proxy.** The base rootfs bakes
  `openssh-server` (key-only root login: `PermitRootLogin prohibit-password`,
  `PasswordAuthentication no`, in `sshd_config.d/sandbox.conf`; host keys via
  `ssh-keygen -A`, so all golden clones share host keys — fine since each is a
  distinct host:port), and `ssh.service` is enabled (socket activation disabled)
  so :22 listens the instant the guest boots. `POST /sandboxes` takes an optional
  `ssh_pubkey` (one OpenSSH key line, `validateSSHPubkey` in server.go — rejects
  multi-line/unknown-type); after the create readiness gate (both cold and hot
  paths), `installSSHKey` (proxy.go) posts it to sandboxd's `POST /ssh-key`, which
  writes `/root/.ssh/authorized_keys`. It is NOT best-effort like `syncGuestClock`:
  a key-install failure destroys the sandbox and fails the create, so a box handed
  back with SSH requested is always reachable. The key lives in the rootfs, so it
  survives hibernation/wake with no re-push. Reach it by exposing guest :22 as a
  host port (`sandbox expose <id> 22`) — the userspace TCP proxy forwards it with
  wake-on-connect, so an incoming SSH connection wakes a hibernated sandbox and
  pins it for the session, exactly like a forwarded HTTP port — then
  `ssh -p <host_port> root@<host>`. Old baked sandboxd 404s `/ssh-key` (re-run
  `install-agent`; rebuild the base for openssh first). **Fleet caveat:** the
  gateway is an HTTP reverse-proxy and does NOT forward raw TCP, so fleet SSH
  needs a ProxyJump to the owning worker (or a WS tunnel) — not wired up yet.
  CLI: `sandbox up --ssh-key ~/.ssh/id_ed25519.pub` (file path or key literal).
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
  (`internal/server/portproxy.go`). The server binds every mapped host port
  (primary + `sandbox_ports` rows) with an in-process listener: accept → record
  activity + pin (same `act.begin` mechanism as API requests) → `ensureRunning` (wakes
  if hibernated) → re-read the row for the CURRENT guest IP (a clone-path wake changes
  it — never cache it) → dial guest → bidirectional copy with TCP half-close. Listeners
  open on create/restore/fanout/expose, persist through hibernation (that's what makes
  wake-on-connect work), re-bind at startup for hibernated rows (`reopenPortListeners`),
  and close on destroy. `RemovePortForward*` (iptables `-D`) is kept and still called in
  destroy/reconcile purely as legacy cleanup for hosts upgrading from the DNAT scheme.
- **Extra port mappings** live in the `sandbox_ports` table and draw host ports from the
  same pool as primary ports (`loadUsed` reads both tables). destroy() and reconcile()
  must close their listeners (and remove legacy DNAT rules) — read mappings before
  deleting rows. `exposePort` works on a hibernated sandbox without waking it: the new
  listener is just another wake-on-connect entry point.
- **`vmCtx` ≠ request ctx.** `handleCreate` must pass `s.vmCtx` (server-scoped) to `vm.NewMachine`
  and `vm.Start`, NOT `r.Context()` — the request ctx cancels when the handler returns, and the
  firecracker SDK SIGTERMs the VM when its ctx cancels. This was an early bug that wasted hours.
- **Pools allocated atomically via SQLite.** `registry.Create` runs INSERT inside a TX with
  partial unique indexes (`uniq_tap_running`, `uniq_ip_running`, `uniq_port_held` — the port
  one also binds `hibernated`) guaranteeing no two sandboxes share a tap/IP/port. Concurrent
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
- Use `modernc.org/sqlite` (pure-Go) NOT `github.com/mattn/go-sqlite3` — we need
  `CGO_ENABLED=0` to cross-compile from macOS.

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
