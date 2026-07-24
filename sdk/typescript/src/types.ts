/**
 * Options accepted by every SDK entry point ({@link Sandbox.create},
 * {@link Sandbox.connect}, ...). Values fall back to the
 * `SANDBOX_API_URL` / `SANDBOX_API_KEY` environment variables.
 */
export interface SandboxOpts {
  /** Base URL of the sandbox API, e.g. `http://100.99.183.74:8080`. Defaults to `SANDBOX_API_URL`. */
  apiUrl?: string
  /** API key sent as `Authorization: Bearer <key>`. Defaults to `SANDBOX_API_KEY`. */
  apiKey?: string
  /** Default per-request timeout in milliseconds (default 30 000; sandbox creation uses 90 000). */
  requestTimeoutMs?: number
}

/**
 * Options shared by every call that brings up a new sandbox
 * ({@link Sandbox.create}, {@link Sandbox.restore}, {@link Sandbox.fanout}).
 */
export interface SandboxBringUpOpts extends SandboxOpts {
  /**
   * Auto-destroy the sandbox after this many milliseconds (rounded up to
   * whole seconds). Omit for no expiry. Can be changed later with
   * `sandbox.setTimeout(ms)`.
   */
  timeoutMs?: number
  /**
   * Override the host's idle-hibernation window for this sandbox, in
   * milliseconds (rounded up to whole seconds): after this long with no API
   * activity the sandbox is frozen to disk and transparently woken by the
   * next request (`status` reads `"hibernated"` while frozen). Pass `-1`
   * to never hibernate. Omit to inherit the host's default.
   */
  hibernateAfterMs?: number
}

/** Options for {@link Sandbox.restore}. */
export interface SandboxRestoreOpts extends SandboxBringUpOpts {
  /**
   * Free-form display name for the restored sandbox. Not unique and not a
   * lookup key — purely a label shown in listings. Can be changed later with
   * `sandbox.rename(name)`.
   */
  name?: string
}

/**
 * Options for {@link Sandbox.fanout}. Unlike a restore, clones are not
 * individually named — the API applies one body to all N of them.
 */
export type SandboxFanoutOpts = SandboxBringUpOpts

/** Options for {@link Sandbox.create}. */
export interface SandboxCreateOpts extends SandboxRestoreOpts {
  /**
   * Number of vCPUs for this sandbox. Omit for the host template's default.
   * Setting this (or {@link memMib}) forces a full cold boot (~2 s) instead
   * of the golden-snapshot hot path (~250 ms) — snapshots bake vcpus/mem at
   * snapshot time, so an override can't be served from one.
   */
  vcpus?: number
  /**
   * Guest memory in MiB for this sandbox. Omit for the host template's
   * default. Same cold-boot cost as {@link vcpus}.
   */
  memMib?: number
  /**
   * A single OpenSSH public key line (e.g. `ssh-ed25519 AAAA… me@laptop`)
   * installed as `/root/.ssh/authorized_keys` in the guest, enabling
   * key-only root SSH. Reach it by exposing guest port 22:
   *
   * ```ts
   * const sbx = await Sandbox.create({ sshPubkey: await readFile('~/.ssh/id_ed25519.pub', 'utf8') })
   * const addr = await sbx.exposePort(22)   // → "100.75.186.35:5200"
   * // ssh -p 5200 root@100.75.186.35
   * ```
   *
   * The key lives in the rootfs, so it survives hibernation and wake. Unlike
   * most create-time extras this is **not** best-effort: if the key can't be
   * installed the sandbox is destroyed and the create fails, so a sandbox
   * handed back with SSH requested is always reachable.
   */
  sshPubkey?: string
}

/** Raw sandbox object as returned by the REST API (snake_case). */
export interface ApiSandbox {
  id: string
  name?: string
  pid: number
  vm_id: string
  socket_path: string
  tap_device: string
  guest_ip: string
  rootfs_path: string
  status: string
  created_at: string
  expires_at?: string
  hibernate_after_sec?: number
  /**
   * Effective vCPU count — current servers always send it (the template
   * default is filled in when no override was set). Absent only with older
   * servers that omitted unset overrides.
   */
  vcpus?: number
  /** Effective guest memory in MiB; same presence rules as {@link vcpus}. */
  mem_mib?: number
  base_snapshot_id?: string
  host_addr?: string
}

/** Raw host info as returned by `GET /info` (snake_case). */
export interface ApiHostInfo {
  default_vcpus: number
  default_mem_mib: number
  max_vcpus: number
  max_mem_mib: number
  hot_create: boolean
  hibernate_after_sec: number
  host_id?: string
}

/** Host template defaults and limits, as returned by {@link Sandbox.hostInfo}. */
export interface HostInfo {
  /** vCPUs a sandbox runs with when created without a `vcpus` override. */
  defaultVcpus: number
  /** Guest memory in MiB when created without a `memMib` override. */
  defaultMemMib: number
  /** Largest accepted per-sandbox `vcpus` override. */
  maxVcpus: number
  /** Largest accepted per-sandbox `memMib` override. */
  maxMemMib: number
  /** Whether creates are served from a pre-booted golden snapshot. */
  hotCreate: boolean
  /** Host default idle-hibernation window in seconds (0 = disabled). */
  hibernateAfterSec: number
  /** Host identity in fleet mode; absent standalone. */
  hostId?: string
}

/** Raw fleet host entry as returned by the gateway's `GET /hosts` (snake_case). */
export interface ApiFleetHost {
  id: string
  addr: string
  slots_total: number
  slots_used: number
  hibernated: number
  free: number
  alive: boolean
  last_seen_ms_ago: number
}

/**
 * One host in the fleet, as reported by {@link Sandbox.hosts}. Gateway-only:
 * a single host has no fleet view of itself.
 */
export interface FleetHostInfo {
  /** Host identity, as it registers itself with the gateway. */
  hostId: string
  /** Address the gateway proxies to, and where this host's forwarded ports live. */
  addr: string
  /** Slots this host advertises in total (its tap/IP pool size). */
  slotsTotal: number
  /** Slots occupied by running sandboxes. */
  slotsUsed: number
  /** Hibernated sandboxes on this host — addressable, but holding no slot. */
  hibernated: number
  /**
   * Slots the gateway will actually place onto: tap/IP availability bounded by
   * memory admission, so it can be lower than `slotsTotal - slotsUsed` when
   * large-memory sandboxes are running.
   */
  free: number
  /** Whether the host's heartbeat is still within the gateway's TTL. */
  alive: boolean
  /** Age of the host's last heartbeat in milliseconds. */
  lastSeenMsAgo: number
}

/** Raw port mapping as returned by the REST API (snake_case). */
export interface ApiPortMapping {
  guest_port: number
  host_port: number
}

/** Raw snapshot object as returned by the REST API (snake_case). */
export interface ApiSnapshot {
  id: string
  name?: string
  source_id: string
  tap_device: string
  guest_ip: string
  mem_path: string
  state_path: string
  rootfs_path: string
  created_at: string
  golden?: boolean
  format?: string
  base_id?: string
  vcpus?: number
  mem_mib?: number
}

/** A saved point-in-time image of a sandbox that can be restored. */
export interface SnapshotInfo {
  /** Unique snapshot id (pass to {@link Sandbox.restore}). */
  snapshotId: string
  /** Display name; absent when the snapshot is unnamed. */
  name?: string
  /** Id of the sandbox this snapshot was taken from. */
  sourceId: string
  /** Creation time. */
  createdAt: Date
  /**
   * True for the server-managed pristine snapshot that hot creates are cloned
   * from (at most one per host). It shows up in {@link Sandbox.listSnapshots}
   * like any other snapshot — hide or badge it in a UI, and don't delete it:
   * creates fall back to cold boot until the server next restarts.
   */
  golden?: boolean
  /**
   * How the artifacts are stored: `'full'` (self-contained) or `'diff'` (a
   * delta against {@link baseId}, which must still exist to restore). Absent
   * on snapshots taken before the diff format existed — treat as `'full'`.
   */
  format?: 'full' | 'diff'
  /** Snapshot this one is a delta against; absent when `format` is `'full'`. */
  baseId?: string
  /**
   * vCPUs baked into the snapshot. Firecracker fixes resources at snapshot
   * time, so every restore and clone runs with these — which is why
   * {@link Sandbox.restore} and {@link Sandbox.fanout} reject overrides.
   * Absent means the source ran the host template's default.
   */
  vcpus?: number
  /** Guest memory in MiB baked into the snapshot; same presence rules as {@link vcpus}. */
  memMib?: number
}

/** One forwarded port: guest port → host port. */
export interface PortMapping {
  /** Port inside the guest. */
  guestPort: number
  /** Host port forwarding to it. */
  hostPort: number
}

/** Information about a sandbox, as returned by {@link Sandbox.list}. */
export interface SandboxInfo {
  /** Unique sandbox id. */
  sandboxId: string
  /** Display name; absent when the sandbox is unnamed. */
  name?: string
  /** Host PID of the firecracker process. */
  pid: number
  /** Firecracker VM id. */
  vmId: string
  /** Host tap device backing the sandbox network. */
  tapDevice: string
  /** IP of the guest on the sandbox bridge. */
  guestIp: string
  /** Path of the per-VM rootfs copy on the host. */
  rootfsPath: string
  /** Firecracker API socket path on the host. */
  socketPath: string
  /** Sandbox status: `"running"`, or `"hibernated"` (frozen to disk; the next request wakes it). */
  status: string
  /** Creation time. */
  createdAt: Date
  /** When the sandbox will be auto-destroyed; absent when it has no TTL. */
  expiresAt?: Date
  /**
   * Per-sandbox idle-hibernation window in seconds (-1 = never hibernate);
   * absent when the sandbox inherits the host default.
   */
  hibernateAfterSec?: number
  /**
   * Effective vCPU count the sandbox runs with. Current servers always
   * report it (the template default is filled in when no override was set);
   * absent only when talking to an older server — treat that as "template
   * default" and look it up via {@link Sandbox.hostInfo}.
   */
  vcpus?: number
  /** Effective guest memory in MiB; same presence rules as {@link vcpus}. */
  memMib?: number
  /**
   * Golden snapshot this sandbox was cloned from (a hot create). Absent for
   * cold boots, restores, and fan-out clones. Its presence is what makes a
   * snapshot of this sandbox storable as a space-efficient diff.
   */
  baseSnapshotId?: string
  /**
   * Address of the machine hosting this sandbox. Set when talking to a fleet
   * gateway (forwarded ports live on the host, not the gateway); absent when
   * talking to a host directly, where the API hostname already is the host.
   */
  hostAddr?: string
}

/** Result of a command executed via `sandbox.commands.run()`. */
export interface CommandResult {
  /** Captured standard output. */
  stdout: string
  /** Captured standard error. */
  stderr: string
  /** Exit code of the command (always 0 here — non-zero exits throw {@link CommandExitError}). */
  exitCode: number
  /** Wall-clock duration of the command in milliseconds. */
  durationMs: number
}

/** Options for `sandbox.commands.run()`. */
export interface CommandRunOpts {
  /** Working directory inside the guest (default `/home/sandbox/app`). */
  cwd?: string
  /** Extra environment variables for the command. */
  envs?: Record<string, string>
  /** Time budget for the command in milliseconds (default 60 000). */
  timeoutMs?: number
  /**
   * Called with each stdout chunk as the command produces it. Providing
   * `onStdout` or `onStderr` switches to the streaming exec endpoint; the
   * returned {@link CommandResult} still carries the full output.
   */
  onStdout?: (data: string) => void
  /** Called with each stderr chunk as the command produces it. */
  onStderr?: (data: string) => void
}

/** A directory entry returned by `sandbox.files.list()`. */
export interface EntryInfo {
  /** Base name of the entry. */
  name: string
  /** Whether the entry is a regular file or a directory. */
  type: 'file' | 'dir'
  /** Size in bytes. */
  size: number
  /** Unix mode string, e.g. `-rw-r--r--`. */
  mode: string
  /** Last modification time. */
  modifiedAt: Date
}

/** Result of `sandbox.files.write()`. */
export interface WriteInfo {
  /** Absolute path that was written inside the guest. */
  path: string
  /** Number of bytes written. */
  bytes: number
}

/** Options for `sandbox.files.read()`. */
export interface ReadOpts {
  /** `'text'` (default) decodes the file as UTF-8; `'bytes'` returns a `Uint8Array`. */
  format?: 'text' | 'bytes'
}

/** Converts a raw API snapshot object to the public {@link SnapshotInfo} shape. */
export function toSnapshotInfo(raw: ApiSnapshot): SnapshotInfo {
  const info: SnapshotInfo = {
    snapshotId: raw.id,
    sourceId: raw.source_id,
    createdAt: new Date(raw.created_at),
  }
  if (raw.name) info.name = raw.name
  if (raw.golden) info.golden = true
  if (raw.format === 'full' || raw.format === 'diff') info.format = raw.format
  if (raw.base_id) info.baseId = raw.base_id
  if (raw.vcpus) info.vcpus = raw.vcpus
  if (raw.mem_mib) info.memMib = raw.mem_mib
  return info
}

/** Converts a raw gateway host entry to the public {@link FleetHostInfo} shape. */
export function toFleetHostInfo(raw: ApiFleetHost): FleetHostInfo {
  return {
    hostId: raw.id,
    addr: raw.addr,
    slotsTotal: raw.slots_total,
    slotsUsed: raw.slots_used,
    hibernated: raw.hibernated,
    free: raw.free,
    alive: raw.alive,
    lastSeenMsAgo: raw.last_seen_ms_ago,
  }
}

/** Converts a raw API host info object to the public {@link HostInfo} shape. */
export function toHostInfo(raw: ApiHostInfo): HostInfo {
  const info: HostInfo = {
    defaultVcpus: raw.default_vcpus,
    defaultMemMib: raw.default_mem_mib,
    maxVcpus: raw.max_vcpus,
    maxMemMib: raw.max_mem_mib,
    hotCreate: raw.hot_create,
    hibernateAfterSec: raw.hibernate_after_sec,
  }
  if (raw.host_id) info.hostId = raw.host_id
  return info
}

/** Converts a raw API sandbox object to the public {@link SandboxInfo} shape. */
export function toSandboxInfo(raw: ApiSandbox): SandboxInfo {
  const info: SandboxInfo = {
    sandboxId: raw.id,
    pid: raw.pid,
    vmId: raw.vm_id,
    tapDevice: raw.tap_device,
    guestIp: raw.guest_ip,
    rootfsPath: raw.rootfs_path,
    socketPath: raw.socket_path,
    status: raw.status,
    createdAt: new Date(raw.created_at),
  }
  if (raw.name) info.name = raw.name
  if (raw.expires_at) info.expiresAt = new Date(raw.expires_at)
  if (raw.hibernate_after_sec) info.hibernateAfterSec = raw.hibernate_after_sec
  if (raw.vcpus) info.vcpus = raw.vcpus
  if (raw.mem_mib) info.memMib = raw.mem_mib
  if (raw.base_snapshot_id) info.baseSnapshotId = raw.base_snapshot_id
  if (raw.host_addr) info.hostAddr = raw.host_addr
  return info
}
