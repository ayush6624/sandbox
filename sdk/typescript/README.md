# websandbox (TypeScript SDK)

TypeScript client for the websandbox API — self-hosted Firecracker microVM
sandboxes for frontend development. The API surface mirrors the
[e2b](https://e2b.dev) JavaScript SDK so it works as a near drop-in replacement.

- Zero runtime dependencies (uses global `fetch`)
- ESM, strict TypeScript, Node 18+

## Install

```bash
npm install            # from this directory, for development
# or, once published / via a file path:
npm install websandbox
```

## Configuration

The SDK reads two environment variables (both can also be passed
programmatically via the `opts` argument on every entry point):

```bash
export WEBSANDBOX_API_URL=http://100.99.183.74:8080
export WEBSANDBOX_API_KEY=<your-key>
```

## Usage

```ts
import { Sandbox } from 'websandbox'

const sbx = await Sandbox.create()              // boots a microVM, ~2s
console.log(sbx.sandboxId)

// Run commands (bash -lc, cwd defaults to /home/sandbox/app)
const res = await sbx.commands.run('node --version', {
  cwd: '/home/sandbox/app',
  envs: { CI: 'true' },
  timeoutMs: 30_000,
})
console.log(res.stdout, res.exitCode, res.durationMs)

// Files
await sbx.files.write('/home/sandbox/app/src/App.tsx', '...code...')
const text = await sbx.files.read('/home/sandbox/app/src/App.tsx')
const bytes = await sbx.files.read('/home/sandbox/app/logo.png', { format: 'bytes' })
const entries = await sbx.files.list('/home/sandbox/app/src')

// Each sandbox serves a Vite dev server on guest port 5173,
// forwarded to a dedicated port on the API host:
const host = sbx.getHost(5173)                  // e.g. "100.99.183.74:5200"
await fetch(`http://${host}/`)

// Lifecycle
const all = await Sandbox.list()
const again = await Sandbox.connect(sbx.sandboxId)
await sbx.kill()                                // or: await Sandbox.kill(id)
```

### Errors

All errors extend `SandboxError`:

| Class | Thrown when |
| --- | --- |
| `AuthenticationError` | API responds 401/403 (bad or missing key) |
| `NotFoundError` | API responds 404 (unknown sandbox, missing file) |
| `TimeoutError` | a command hits its `timeoutMs` budget, or an HTTP request times out |
| `CommandExitError` | a command exits non-zero; carries the full `CommandResult` (`.exitCode`, `.stdout`, `.stderr`, `.result`) |

## Migrating from e2b

| e2b | websandbox |
| --- | --- |
| `import { Sandbox } from '@e2b/code-interpreter'` | `import { Sandbox } from 'websandbox'` |
| `Sandbox.create('template', opts)` | `Sandbox.create(opts)` — single built-in Vite React-TS template |
| `Sandbox.connect(id)` | `Sandbox.connect(id)` |
| `Sandbox.list()` | `Sandbox.list()` |
| `Sandbox.kill(id)` | `Sandbox.kill(id)` |
| `sbx.sandboxId` | `sbx.sandboxId` |
| `sbx.commands.run(cmd, { cwd, envs, timeoutMs })` | `sbx.commands.run(cmd, { cwd, envs, timeoutMs })` |
| `sbx.files.write(path, data)` | `sbx.files.write(path, data)` |
| `sbx.files.read(path)` / `read(path, { format: 'bytes' })` | same |
| `sbx.files.list(path)` | `sbx.files.list(path)` |
| `sbx.getHost(port)` | `sbx.getHost(5173)` — only guest port 5173 is forwarded |
| `sbx.kill()` | `sbx.kill()` |
| `E2B_API_KEY` env var | `WEBSANDBOX_API_KEY` (+ `WEBSANDBOX_API_URL`) |
| `CommandExitError` / `TimeoutError` | same names and semantics |

Not supported (yet): background/streaming commands (`commands.run(..., { background: true })`,
`onStdout`/`onStderr`), PTYs, `files.watchDir`, sandbox metadata/templates, pause/resume,
and per-sandbox timeouts — sandboxes live until killed.

## Scripts

```bash
npm run typecheck   # tsc --noEmit over src/, examples/, test/
npm run build       # emit dist/ (JS + .d.ts)
npm test            # mock-server smoke test (node:test via tsx)
npm run example     # run examples/demo.ts against a live server
```

## Example

`examples/demo.ts` exercises the full loop: create a sandbox, check
`node`/`pnpm` versions, write a React component into the Vite app, read it
back, list the src directory, poll the forwarded Vite dev server until it
serves HTML, list sandboxes, and kill the sandbox.

```bash
WEBSANDBOX_API_URL=http://<host>:8080 WEBSANDBOX_API_KEY=<key> npm run example
```
