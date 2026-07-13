# sandbox (TypeScript SDK)

TypeScript client for the sandbox API — self-hosted Firecracker microVM
sandboxes for frontend development. The API surface mirrors the
[e2b](https://e2b.dev) JavaScript SDK so it works as a near drop-in replacement.

- Zero runtime dependencies (uses global `fetch`)
- ESM, strict TypeScript, Node 18+

## Install

Published as a tarball on [GitHub Releases](https://github.com/ayush6624/sandbox/releases)
(tags `sdk-v*`):

```bash
npm install https://github.com/ayush6624/sandbox/releases/download/sdk-v0.1.0/sandbox-0.1.0.tgz
```

Upgrading means pointing at a newer release URL — there are no semver ranges
with tarball installs.

For development, work from this directory directly:

```bash
npm install && npm run build
```

To cut a release: bump `version` in package.json, then
`npm pack` (builds via `prepack`) and
`gh release create sdk-v<version> sandbox-<version>.tgz`.

## Configuration

The SDK reads two environment variables (both can also be passed
programmatically via the `opts` argument on every entry point):

```bash
export SANDBOX_API_URL=http://100.99.183.74:8080
export SANDBOX_API_KEY=<your-key>
```

## Usage

```ts
import { Sandbox } from 'sandbox'

const sbx = await Sandbox.create()              // ready in a few hundred ms
console.log(sbx.sandboxId)

// Run commands (bash -lc, cwd defaults to /home/sandbox/app)
const res = await sbx.commands.run('node --version', {
  cwd: '/home/sandbox/app',
  envs: { CI: 'true' },
  timeoutMs: 30_000,
})
console.log(res.stdout, res.exitCode, res.durationMs)

// Stream output as it is produced (e2b-style onStdout/onStderr).
// Passing either callback switches to the streaming endpoint; the returned
// CommandResult still carries the full accumulated output, and
// CommandExitError / TimeoutError semantics are identical to the buffered path.
await sbx.commands.run('pnpm install', {
  onStdout: (chunk) => process.stdout.write(chunk),
  onStderr: (chunk) => process.stderr.write(chunk),
  timeoutMs: 120_000,
})

// Files
await sbx.files.write('/home/sandbox/server.js', '...code...')
const text = await sbx.files.read('/home/sandbox/server.js')
const bytes = await sbx.files.read('/home/sandbox/logo.png', { format: 'bytes' })
const entries = await sbx.files.list('/home/sandbox')

// Guest port 3000 is forwarded to a dedicated port on the API host at create
// time; start a server there (nothing runs by default) and reach it via:
const host = sbx.getHost(3000)                  // e.g. "100.99.183.74:5200"
await fetch(`http://${host}/`)

// Expose additional guest ports on demand. exposePort is idempotent and
// returns the externally reachable "host:port"; afterwards the synchronous
// getHost(port) works for that port too (it reads a per-instance cache —
// for ports exposed elsewhere, call listPorts() first to refresh it).
const api = await sbx.exposePort(8000)          // e.g. "100.99.183.74:5201"
const ports = await sbx.listPorts()             // [{ guestPort: 3000, hostPort: 5200 }, { guestPort: 8000, hostPort: 5201 }]
sbx.getHost(8000)                               // works now; throws for unexposed ports

// Lifecycle
const all = await Sandbox.list()
const again = await Sandbox.connect(sbx.sandboxId)
await sbx.kill()                                // or: await Sandbox.kill(id)
```

### Auto-destroy (TTL)

Sandboxes live until killed unless you give them a timeout. The server reaps
expired sandboxes within ~10 seconds of their deadline.

```ts
const sbx = await Sandbox.create({ timeoutMs: 300_000 })  // auto-destroy in 5 min
console.log(sbx.info.expiresAt)                           // Date | undefined

await sbx.setTimeout(600_000)   // replace the timeout: now 10 min from now
await sbx.setTimeout(0)         // remove the timeout entirely
```

`timeoutMs` is rounded up to whole seconds (the API speaks `timeout_sec`).

### Resource overrides

Sandboxes default to the host template's vCPUs and memory. Override either
per sandbox at create time:

```ts
const big = await Sandbox.create({ vcpus: 4, memMib: 4096 })
console.log(big.info.vcpus, big.info.memMib)  // 4 4096 (absent = template default)
```

An override forces a full cold boot (~2 s) instead of the golden-snapshot hot
path (~250 ms): Firecracker bakes vcpus/mem into the snapshot at snapshot
time, so an override can't be served from one. Overrides also can't be passed
to `restore`/`fanout` — a restored sandbox always runs with the resources
baked into its snapshot.

### Snapshots, restore, and fan-out

A snapshot captures a running sandbox completely — memory, running processes,
and disk. Restoring one brings all of that back in a new sandbox in a few
hundred milliseconds: a dev server that took a minute of `pnpm install` to
reach is ready instantly, mid-request-handler if that's when you snapshotted.

```ts
// Prepare state once...
const base = await Sandbox.create()
await base.commands.run('git clone https://github.com/you/app && cd app && pnpm install')
const snap = await base.snapshot()      // pauses briefly, then keeps running
await base.kill()                       // source must be gone before restoring

// ...restore it whenever you need it back (1:1, at most one at a time)
const sbx = await Sandbox.restore(snap.snapshotId)

// ...or fan out N independent clones of it, concurrently
const clones = await Sandbox.fanout(snap.snapshotId, 32, { timeoutMs: 600_000 })
// each clone: own IP/ports, copy-on-write disk — writes are isolated

// Housekeeping
const snaps = await Sandbox.listSnapshots()
await Sandbox.deleteSnapshot(snap.snapshotId)
```

`restore` reuses the network identity baked into the snapshot, so only one
restore of a given snapshot can run at a time. `fanout` gives every clone a
fresh identity, so any number can run side by side (measured: 32 clones in
~2.7 s, sharing memory/disk state, isolated writes).

Note: you don't need snapshots just to make `create` fast — the server keeps
a golden snapshot of a freshly booted sandbox and serves plain `create` from
it automatically. Snapshots are for capturing *your* prepared state.

Multi-host works transparently: when `SANDBOX_API_URL` points at a gateway
(fleet mode), `sbx.snapshot()` is routed to the sandbox's host,
`listSnapshots` merges across hosts, and `restore` / `fanout` /
`deleteSnapshot` are forwarded to the snapshot's owning host (or, with GCS
durability configured, to any live host if the owner is gone).

### Errors

All errors extend `SandboxError`:

| Class | Thrown when |
| --- | --- |
| `AuthenticationError` | API responds 401/403 (bad or missing key) |
| `NotFoundError` | API responds 404 (unknown sandbox, missing file) |
| `TimeoutError` | a command hits its `timeoutMs` budget, or an HTTP request times out |
| `CommandExitError` | a command exits non-zero; carries the full `CommandResult` (`.exitCode`, `.stdout`, `.stderr`, `.result`) |

## Migrating from e2b

| e2b | sandbox |
| --- | --- |
| `import { Sandbox } from '@e2b/code-interpreter'` | `import { Sandbox } from 'sandbox'` |
| `Sandbox.create('template', { timeoutMs })` | `Sandbox.create({ timeoutMs, ...opts })` — single built-in Node + Python template |
| `Sandbox.connect(id)` | `Sandbox.connect(id)` |
| `Sandbox.list()` | `Sandbox.list()` |
| `Sandbox.kill(id)` | `Sandbox.kill(id)` |
| `sbx.sandboxId` | `sbx.sandboxId` |
| `sbx.commands.run(cmd, { cwd, envs, timeoutMs })` | `sbx.commands.run(cmd, { cwd, envs, timeoutMs })` |
| `sbx.commands.run(cmd, { onStdout, onStderr })` | same — streams chunks, still returns the full result |
| `sbx.setTimeout(ms)` | `sbx.setTimeout(ms)` — `0` clears the timeout |
| `sbx.files.write(path, data)` | `sbx.files.write(path, data)` |
| `sbx.files.read(path)` / `read(path, { format: 'bytes' })` | same |
| `sbx.files.list(path)` | `sbx.files.list(path)` |
| `sbx.getHost(port)` | `sbx.getHost(port)` — 3000 always works; other ports after `await sbx.exposePort(port)` |
| — | `sbx.exposePort(guestPort)` / `sbx.listPorts()` |
| `sbx.kill()` | `sbx.kill()` |
| `E2B_API_KEY` env var | `SANDBOX_API_KEY` (+ `SANDBOX_API_URL`) |
| `CommandExitError` / `TimeoutError` | same names and semantics |
| `sbx.betaPause()` / resume | `sbx.snapshot()` + `Sandbox.restore(snapshotId)` — full memory+disk capture |
| — | `Sandbox.fanout(snapshotId, n)` — N live clones of one snapshot |

Not supported (yet): background commands (`commands.run(..., { background: true })`),
PTYs from the SDK (`sandbox shell <id>` in the CLI covers interactive use),
`files.watchDir`, and sandbox metadata/templates.

## Scripts

```bash
npm run typecheck   # tsc --noEmit over src/, examples/, test/
npm run build       # emit dist/ (JS + .d.ts)
npm test            # mock-server smoke test (node:test via tsx)
npm run example     # run examples/demo.ts against a live server
```

## Examples

Each script in `examples/` runs against a live server; all read the
`SANDBOX_API_URL` / `SANDBOX_API_KEY` env vars:

| Script | Shows |
| --- | --- |
| `npm run example` | The broad tour: create, exec, write/read/list files, list, kill |
| `npm run example:streaming` | Streaming exec (`onStdout`/`onStderr` chunks) and `CommandExitError` on non-zero exit |
| `npm run example:ports` | Start a server in the guest, `exposePort`, reach it via `getHost`, `listPorts` |
| `npm run example:lifecycle` | `create({ timeoutMs })`, `setTimeout`, `Sandbox.list`, `Sandbox.connect` by id, `kill` |
| `npm run example:speed` | Hot-create latency: sequential + concurrent creates, first exec round-trip |
| `npm run example:fanout` | Snapshot → fan out N clones: shared prepared state, surviving processes, isolated writes (needs a host URL, not a gateway) |

```bash
export SANDBOX_API_URL=http://<host>:8080
export SANDBOX_API_KEY=<key>
npm run example:ports
```
