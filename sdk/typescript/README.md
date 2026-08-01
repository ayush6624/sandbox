# sandbox (TypeScript SDK)

TypeScript client for the versioned sandbox API — self-hosted Firecracker
microVM development environments with resource-oriented lifecycle methods.

- Zero runtime dependencies (uses global `fetch`)
- ESM, strict TypeScript, Node 18+

## Install

Published as a tarball on [GitHub Releases](https://github.com/ayush6624/sandbox/releases)
(tags `sdk-v*`):

```bash
npm install https://github.com/ayush6624/sandbox/releases/download/sdk-v2.0.0/sandbox-2.0.0.tgz
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

Fleet operators additionally use `SANDBOX_CONTROL_KEY` (and optionally
`SANDBOX_CONTROL_URL`) for [`FleetClient`](#fleet-capacity-operator-api). That
credential is a different trust domain from `SANDBOX_API_KEY` and is never read
by the tenant clients.

## Resource client

`SandboxClient` is the primary API. It uses the versioned `/v1` contract,
generates idempotency keys for mutations, safely retries retryable requests,
and exposes paginated collections as `AsyncIterable`.

```ts
import { SandboxClient } from 'sandbox'

const client = new SandboxClient({
  baseUrl: process.env.SANDBOX_API_URL,
  apiKey: process.env.SANDBOX_API_KEY,
})

const sandbox = await client.sandboxes.create({
  source: { templateId: 'default' },
  ttlMs: 10 * 60_000,
  idleTimeoutMs: 5 * 60_000,
  metadata: { project: 'docs' },
})

await sandbox.commands.run('node --version')
await sandbox.pause()
await sandbox.resume()
await sandbox.terminate()
```

Create several independent sandboxes from one reusable snapshot with a typed
operation. Every requested index has either a sandbox or structured problem
details; partial success is never represented by a mysteriously short array.

```ts
const prepared = await client.sandboxes.create()
await prepared.commands.run('pnpm install')
const snapshot = await prepared.createSnapshot({ name: 'dependencies-ready' })

const operation = await client.sandboxes.createMany({
  count: 32,
  source: { snapshotId: snapshot.id },
  maxParallelism: 8,
})
const abortController = new AbortController()
const result = await operation.wait({ signal: abortController.signal })
for (const item of result.results) {
  if (item.value) console.log(item.index, item.value.id)
  else console.error(item.index, item.error?.code)
}
```

Collections can be streamed without manually handling page tokens:

```ts
for await (const sandbox of client.sandboxes.list({ status: 'running' })) {
  console.log(sandbox.id, sandbox.metadata)
}
```

## Compatibility facade

The original static `Sandbox` surface remains available during the migration
window. New code should use `pause`, `resume`, and `terminate`; `hibernate` and
`kill` remain deprecated aliases. `restore` and `fanout` are also deprecated in
favor of source creation and `createMany`.

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

// Ports are private until explicitly exposed. Start a server (nothing runs by
// default), expose its guest port, and use the returned address:
const host = await sbx.exposePort(3000)         // e.g. "100.99.183.74:5200"
await fetch(`http://${host}/`)

// exposePort is idempotent and
// returns the externally reachable "host:port"; afterwards the synchronous
// getHost(port) works for that port too (it reads a per-instance cache —
// for ports exposed elsewhere, call listPorts() first to refresh it).
const api = await sbx.exposePort(8000)          // e.g. "100.99.183.74:5201"
const ports = await sbx.listPorts()             // only explicitly exposed mappings
sbx.getHost(8000)                               // works now; throws for unexposed ports

// Lifecycle (standard names)
const all = await Sandbox.list()
const again = await Sandbox.connect(sbx.sandboxId)
await sbx.pause()
await sbx.resume()
await sbx.terminate()                           // or: await Sandbox.terminate(id)
```

### Auto-destroy (TTL)

Sandboxes live until killed unless you give them a timeout. The server reaps
expired sandboxes within ~10 seconds of their deadline.

```ts
const sbx = await Sandbox.create({ ttlMs: 300_000 })      // auto-destroy in 5 min
console.log(sbx.info.expiresAt)                           // Date | undefined

await sbx.setTimeout(600_000)   // replace the timeout: now 10 min from now
await sbx.setTimeout(0)         // remove the timeout entirely
```

`ttlMs` is rounded up to whole seconds. `timeoutMs` remains a deprecated alias;
command `timeoutMs` and HTTP `requestTimeoutMs` are separate budgets.

### Pause and resume

Idle sandboxes are frozen to disk automatically — **the shipped hosts do this
after 10 minutes** — and woken transparently by the next command, file
operation, shell, or connection to a forwarded port. Memory and running
processes come back exactly as they were (~50 ms typical), so this is a cost
optimization, not a lifecycle event you have to handle:

```ts
const sbx = await Sandbox.create({ idleTimeoutMs: 30 * 60_000 })
await sbx.pause()
await sbx.resume()
```

What counts as activity is **external** traffic: API calls, an open shell or
exec stream, and connections through exposed ports. Work happening only inside
the guest does not — a detached `tmux` job can be hibernated mid-run. It
resumes on the next request, but external TCP sessions it held may have timed
out. `hibernateAfterMs` and `hibernate()` remain compatibility aliases.

Hibernation is independent of `timeoutMs`: freezing is recoverable, while the
TTL destroys the sandbox whether it is running or hibernated.

### Names

Sandboxes and snapshots take an optional free-form display name — a label
for listings, not a unique id or lookup key:

```ts
const sbx = await Sandbox.create({ name: 'my devbox' })
console.log(sbx.info.name)                       // 'my devbox'
await sbx.rename('renamed')                      // '' clears the name

const snap = await sbx.snapshot({ name: 'deps-installed' })
await Sandbox.renameSnapshot(snap.snapshotId, 'prepped')
```

`restore` also accepts `{ name }` for the new sandbox it boots. Names are
capped at 64 bytes; unnamed objects surface `info.name === undefined`.

### Resource overrides

Sandboxes default to the host template's vCPUs and memory. Override either
per sandbox at create time:

```ts
const big = await Sandbox.create({ vcpus: 4, memMib: 4096 })
console.log(big.info.vcpus, big.info.memMib)  // 4 4096
```

`info.vcpus` / `info.memMib` always report the **effective** resources: with
no override, the server fills in the template defaults (e.g. `2 1024`), so a
dashboard never has to guess. The defaults and the accepted override bounds
are queryable:

```ts
const host = await Sandbox.hostInfo()
console.log(host.defaultVcpus, host.defaultMemMib)  // 2 1024
console.log(host.maxVcpus, host.maxMemMib)          // e.g. 16 64312
```

An override forces a full cold boot (~2 s) instead of the golden-snapshot hot
path (~250 ms): Firecracker bakes vcpus/mem into the snapshot at snapshot
time, so an override can't be served from one. Overrides also can't be passed
to `restore`/`fanout` — a restored sandbox always runs with the resources
baked into its snapshot.

### Interactive PTY

`sandbox.pty` opens a real login shell (`bash -l` on a pty) over the shell
WebSocket — the same protocol the CLI's `sandbox shell <id>` speaks:

```ts
const pty = await sbx.pty.create({
  cols: 120,
  rows: 30,
  onData: (data) => process.stdout.write(data),   // raw terminal bytes
})
pty.sendInput('ls -la\n')
pty.resize({ cols: 200, rows: 50 })
pty.sendInput('exit\n')
console.log('exit code:', await pty.exited)       // or pty.kill() to end it
```

Requires a global `WebSocket` (Node 22+, or any browser). The API key is sent
in the WebSocket subprotocol list, not the URL — nothing extra to configure,
and it works unchanged in a browser, where request headers can't be set.
Requires a server at or after the `6e4f1c0` management-security change; older
hosts expect a query credential and will refuse the upgrade.

Auth and routing failures reject with the same typed errors as the REST API —
the server delivers them as WebSocket close codes (`4401` bad key →
`AuthenticationError`, `4404` unknown sandbox → `NotFoundError`) instead of
an opaque `1006`. Connecting to a hibernated sandbox wakes it transparently.

### Snapshots and batch creation

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

const operation = await Sandbox.createMany({
  count: 32,
  source: { snapshotId: snap.snapshotId },
  ttlMs: 600_000,
})
const clones = await operation.wait()

// Housekeeping
const snaps = await Sandbox.listSnapshots()
await Sandbox.renameSnapshot(snap.snapshotId, 'deps-installed')
await Sandbox.deleteSnapshot(snap.snapshotId)
```

Batch creation records one indexed result for every requested sandbox, with
structured errors for failures. The old `restore` and `fanout` methods remain
deprecated compatibility calls for one migration window.

`listSnapshots()` also returns the server's **golden** snapshot, flagged
`golden: true` — the pristine image plain `create` clones from. Hide or badge
it in a UI, and don't delete it: creates fall back to cold boot until the
server next restarts. A snapshot whose `format` is `'diff'` is stored as a
delta against `baseId`, which must still exist for it to restore.

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

### SSH into a sandbox

Pass an OpenSSH public key at create time and expose guest port 22:

```ts
import { readFile } from 'node:fs/promises'

const sbx = await Sandbox.create({
  sshPubkey: await readFile(`${process.env.HOME}/.ssh/id_ed25519.pub`, 'utf8'),
})
const addr = await sbx.exposePort(22)     // e.g. "100.75.186.35:5200"
const [host, port] = addr.split(':')
console.log(`ssh -p ${port} sandbox@${host}`)
```

Key-only login as the unprivileged `sandbox` user; the key lands in
`/home/sandbox/.ssh/authorized_keys` and survives hibernation and wake.
Independent creates rotate SSH host keys and clear inherited login keys. If
the key can't be installed the create fails outright rather than handing back
an unreachable sandbox.

Because the forwarded port carries wake-on-connect, an incoming SSH connection
wakes a hibernated sandbox and pins it for the session. In fleet mode the
gateway proxies HTTP only, so SSH needs a `ProxyJump` through the owning
worker (`sbx.info.hostAddr` names it).

### Fleet capacity (operator API)

Host inventory is **not** a tenant call: it discloses per-host addresses and
live capacity, so the gateway authenticates it with the worker-control
credential. It therefore lives on its own client, `FleetClient`, with its own
credential — a tenant `Sandbox`/`SandboxClient` cannot express the call at all:

```ts
import { FleetClient } from 'sandbox'

// SANDBOX_CONTROL_KEY (the gateway's GATEWAY_CONTROL_TOKEN), and
// SANDBOX_CONTROL_URL or SANDBOX_API_URL for the gateway address.
const fleet = new FleetClient()

for (const h of await fleet.hosts.list()) {
  console.log(h.hostId, `${h.slotsUsed}/${h.slotsTotal} used`, `${h.free} free`,
              h.hibernated, 'hibernated', h.alive ? 'live' : 'stale')
}
```

`free` is what placement trusts: tap/IP availability bounded by memory
admission, so it can be lower than `slotsTotal - slotsUsed` when
large-memory sandboxes are running. Host-only URLs answer 404 here (a single
host has no fleet view of itself), and a tenant API key gets an
`AuthenticationError`.

`Sandbox.hosts()` was removed in 2.0.0 — see the [changelog](CHANGELOG.md).

### Refreshing a handle

`sbx.info` is a snapshot from when the handle was made, and `status` drifts on
its own — the idle reaper hibernates the sandbox, and the next command wakes
it. `refresh()` re-reads the sandbox and updates `info` **in place**, so
references you already handed out stay live:

```ts
await sbx.refresh()
console.log(sbx.info.status)      // "running" | "hibernated"
console.log(sbx.info.expiresAt)   // undefined once the TTL is cleared
```

### Errors

All errors extend `SandboxError`, which carries the HTTP `status` it came from
(WebSocket failures map their `4000 + status` close code back onto it):

| Class | Thrown when |
| --- | --- |
| `AuthenticationError` | API responds 401/403 (bad or missing key) |
| `NotFoundError` | API responds 404 (unknown sandbox, missing file) |
| `ConflictError` | API responds 409 — the sandbox isn't in a state the operation allows (snapshot/hibernate of a sandbox not running on its host, restoring a snapshot whose identity is still in use) |
| `CapacityError` | API responds 429/503 — the fleet is **full, not broken**. Carries `retryAfterMs` when the server sent a `Retry-After`. This is the retryable class: the same call often succeeds shortly after |
| `TimeoutError` | a command hits its `timeoutMs` budget, or an HTTP request times out |
| `CommandExitError` | a command exits non-zero; carries the full `CommandResult` (`.exitCode`, `.stdout`, `.stderr`, `.result`) |

```ts
try {
  return await Sandbox.create()
} catch (err) {
  if (err instanceof CapacityError) {
    await new Promise((r) => setTimeout(r, err.retryAfterMs ?? 5_000))
    return await Sandbox.create()      // the autoscaler may have added a host
  }
  throw err
}
```

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
| `sbx.getHost(port)` | `sbx.getHost(port)` after `await sbx.exposePort(port)` |
| — | `sbx.exposePort(guestPort)` / `sbx.listPorts()` |
| `sbx.kill()` | `sbx.kill()` |
| `E2B_API_KEY` env var | `SANDBOX_API_KEY` (+ `SANDBOX_API_URL`) |
| `CommandExitError` / `TimeoutError` | same names and semantics |
| `sbx.betaPause()` / resume | `sbx.snapshot()` + `Sandbox.restore(snapshotId)` — full memory+disk capture |
| — | `Sandbox.fanout(snapshotId, n)` — N live clones of one snapshot |
| `sbx.pty.create({ onData, cols, rows })` | `sbx.pty.create({ onData, cols, rows, cwd })` — `sendInput` / `resize` / `kill` / `await pty.exited` |
| `sbx.getInfo()` | `sbx.info` (a live object) + `await sbx.refresh()` to re-read it; `sbx.info.vcpus` / `memMib` are always the effective values, and `Sandbox.hostInfo()` gives defaults/limits |
| — | `Sandbox.create({ sshPubkey })` + `exposePort(22)` — key-only SSH as `sandbox` |
| — | `new FleetClient().hosts.list()` — fleet capacity behind a gateway (operator credential, not the tenant API key) |
| rate-limit errors | `CapacityError` (429/503) with `retryAfterMs` — distinguishes "fleet full" from "broken" |

Not supported (yet): background commands (`commands.run(..., { background: true })`),
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
