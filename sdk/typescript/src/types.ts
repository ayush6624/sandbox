/**
 * Options accepted by every SDK entry point ({@link Sandbox.create},
 * {@link Sandbox.connect}, ...). Values fall back to the
 * `WEBSANDBOX_API_URL` / `WEBSANDBOX_API_KEY` environment variables.
 */
export interface SandboxOpts {
  /** Base URL of the websandbox API, e.g. `http://100.99.183.74:8080`. Defaults to `WEBSANDBOX_API_URL`. */
  apiUrl?: string
  /** API key sent as `Authorization: Bearer <key>`. Defaults to `WEBSANDBOX_API_KEY`. */
  apiKey?: string
  /** Default per-request timeout in milliseconds (default 30 000; sandbox creation uses 90 000). */
  requestTimeoutMs?: number
}

/** Raw sandbox object as returned by the REST API (snake_case). */
export interface ApiSandbox {
  id: string
  pid: number
  vm_id: string
  socket_path: string
  tap_device: string
  guest_ip: string
  host_port: number
  rootfs_path: string
  status: string
  created_at: string
}

/** Information about a sandbox, as returned by {@link Sandbox.list}. */
export interface SandboxInfo {
  /** Unique sandbox id. */
  sandboxId: string
  /** Host PID of the firecracker process. */
  pid: number
  /** Firecracker VM id. */
  vmId: string
  /** Host tap device backing the sandbox network. */
  tapDevice: string
  /** IP of the guest on the sandbox bridge. */
  guestIp: string
  /** Host port forwarding to guest port 5173 (the Vite dev server). */
  hostPort: number
  /** Path of the per-VM rootfs copy on the host. */
  rootfsPath: string
  /** Firecracker API socket path on the host. */
  socketPath: string
  /** Sandbox status, e.g. `"running"`. */
  status: string
  /** Creation time. */
  createdAt: Date
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

/** Converts a raw API sandbox object to the public {@link SandboxInfo} shape. */
export function toSandboxInfo(raw: ApiSandbox): SandboxInfo {
  return {
    sandboxId: raw.id,
    pid: raw.pid,
    vmId: raw.vm_id,
    tapDevice: raw.tap_device,
    guestIp: raw.guest_ip,
    hostPort: raw.host_port,
    rootfsPath: raw.rootfs_path,
    socketPath: raw.socket_path,
    status: raw.status,
    createdAt: new Date(raw.created_at),
  }
}
