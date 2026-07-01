# Snapshot fan-out — M0 spike findings

De-risk spike for `~/.claude/plans/snapshot-fanout.md`. Ran hand-driven Firecracker
API experiments on `testvm-1` (Firecracker **v1.15.0**, base rootfs on XFS reflink).
Reproducers: `scripts/spike/spike-a.sh`, `scripts/spike/spike-b.sh`, `scripts/spike/thaw.sh`.

**Bottom line: both unknowns pass. Fan-out is viable with the recipe below, and it
needs the raw Firecracker HTTP API — the Go SDK v1.0.0 helper cannot express it.**

## Unknown #1 — rootfs relocation → **B1 (PATCH /drives), confirmed**

`PUT /snapshot/load {resume_vm:false}` → `PATCH /drives/rootfs {path_on_host:<clone>}`
→ `PATCH /vm {state:"Resumed"}` works. Resume after relocation was **~68 ms**.

Proof of isolation (spike-a): the resumed guest wrote a file; it appeared in the
**clone image only, source untouched** → the guest's block I/O really was redirected
to the relocated path. **We do NOT need B2 (mount-namespace bind).**

Caveat learned: editing the ext4 *offline* after snapshot (host loop-mount) is invisible
to the resumed guest for already-cached metadata/dentries — the guest's page cache is
frozen at snapshot time. Per-clone divergence must come from the guest writing at
runtime (copy-on-write via reflink), not from host-side offline edits. Fine for fan-out.

## Unknown #2 — network reidentification → **works, via `network_overrides` + in-guest MMDS**

The plan assumed no host-side network override exists (true for **Go SDK v1.0.0**) and
put *all* reidentification in-guest. The spike found the raw API is richer:

- **Host tap** is remapped at load with `PUT /snapshot/load {network_overrides:[{iface_id,host_dev_name}]}`.
  `PATCH /network-interfaces/{id}` **cannot** change `host_dev_name` (only rate limiters) —
  that was a dead end. `network_overrides` is the correct primitive and each clone gets its
  own fresh tap this way.
- **Guest IP + MAC** still live in guest memory (network_overrides only fixes the host side),
  so they must be reconfigured *in-guest*. Driven by **MMDS**: push per-clone identity into
  `PUT /mmds` before resume; a guest agent reads it and reconfigures `eth0`.

MMDS gotchas confirmed:
- **V2 requires a session token on every request**, and **GET needs `Accept: application/json`**
  or MMDS returns a plaintext key-listing (not JSON). The guest needs a route:
  `ip route add 169.254.169.254/32 dev eth0`.
- Collision-free sequence validated: create the clone tap **unbridged** → resume → guest reads
  MMDS and sets new IP/MAC → **then** attach tap to `br-fc`. The old (source) IP never appears
  on the shared bridge; after reidentify the old IP is dead, the new IP is reachable.

## The clone recipe (per clone, all raw API on a fresh FC socket)

1. `CloneFile(snapshot.rootfs, cloneRootfs)` — `cp --reflink=always` (instant CoW on XFS).
2. Create the clone's tap **unbridged** (`ip tuntap add … ; ip link set … up`).
3. `PUT /snapshot/load { snapshot_path, mem_backend:{File,…}, resume_vm:false,
   network_overrides:[{iface_id:"eth0", host_dev_name:<cloneTap>}] }`.
4. `PATCH /drives/rootfs { path_on_host:<cloneRootfs> }`.
5. `PUT /mmds { ip, mac, gw, prefix, gen:<bump> }` — the clone's fresh identity.
6. `PATCH /vm { state:"Resumed" }`.
7. Guest thaw-agent detects the gen bump, reconfigures `eth0` (IP+MAC+route).
8. Attach the clone tap to `br-fc`; add DNAT for the host port.
9. Poll the agent on the **new** IP for readiness.

## Implications for M2 (implementation)

- **Bypass the SDK's `WithSnapshot` helper.** `firecracker-go-sdk v1.0.0` loads+resumes in
  one step and exposes neither `network_overrides` nor a load-then-PATCH-then-resume window.
  M2 must call the Firecracker HTTP API directly (unix socket) for the clone path — either raw
  `net/http` over the socket, or the SDK's lower-level client. `internal/vm` needs a
  `LoadForClone`/`ResumeClone` pair rather than `NewMachineFromSnapshot`.
- **MMDS must be enabled on the source** at create time so snapshots carry the MMDS device
  (`PUT /mmds/config {version:"V2", network_interfaces:["eth0"]}` before `InstanceStart`).
  Add to the VM template / `internal/vm` cold-boot path, else existing snapshots can't fan out.
- **Guest thaw-reidentify belongs in `sandboxd`** (spike used a standalone systemd unit): on
  resume, read MMDS (V2 token + `Accept: application/json`), reconfigure `eth0`, signal ready.
  Bake via `install-agent`. Detect resume by polling the MMDS `gen` token.
- **`registry.CreateClone`** allocates fresh tap/IP/port from the pool (like `Create`), ignoring
  the snapshot's baked identity — the partial-unique-index "blocker" dissolves once we stop
  reusing baked tap/IP.
- **Deferred bridge attach** in the provisioner: `CreateTapUnbridged` + `AttachTapToBridge`.
- Memory is still per-clone (each mmaps its own mem file); UFFD dedup stays M3.
