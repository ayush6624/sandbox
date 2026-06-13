# sandbox (TypeScript SDK)

TypeScript client for the sandbox API — self-hosted Firecracker microVM
sandboxes for frontend development. The API surface mirrors the
[e2b](https://e2b.dev) JavaScript SDK so it works as a near drop-in replacement.

- Zero runtime dependencies (uses global `fetch`)
- ESM, strict TypeScript, Node 18+

## Install

```bash
npm install            # from this directory, for development
# or, once published / via a file path:
npm install sandbox
```

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

const sbx = await Sandbox.create()              // boots a microVM, ~2s
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

Not supported (yet): background commands (`commands.run(..., { background: true })`),
PTYs, `files.watchDir`, sandbox metadata/templates, and pause/resume.

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

```bash
export SANDBOX_API_URL=http://<host>:8080
export SANDBOX_API_KEY=<key>
npm run example:ports
```
