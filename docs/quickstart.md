# Quickstart

Create an isolated Linux microVM, run commands in it, and reach a server
running inside — in about five minutes.

Every sandbox is a real VM (Firecracker/KVM) booting Ubuntu 24.04 with Node 22,
pnpm, TypeScript, Python 3, git, and build-essential. Creates are served from a
pre-booted snapshot, so a fresh sandbox is ready in a few hundred milliseconds.

## Prerequisites

You need the address and token of a running sandbox server (or gateway, in
fleet mode). If you don't have one yet, set one up first — see
[Self-hosting](self-hosting.md). Then:

```bash
export SANDBOX_API_URL=http://100.69.9.101:9090   # your server or gateway
export SANDBOX_API_KEY=<token>
```

## Option A: TypeScript SDK (recommended)

```bash
npm install sandbox   # or point at sdk/typescript in this repo
```

```ts
import { Sandbox } from 'sandbox'

// 1. Create a sandbox (auto-destroys in 10 minutes unless you extend it)
const sbx = await Sandbox.create({ timeoutMs: 600_000 })

// 2. Run commands — bash -lc, default cwd /home/sandbox/app
const res = await sbx.commands.run('node --version && python3 --version')
console.log(res.stdout)        // v22.x / Python 3.12.x

// 3. Write and read files
await sbx.files.write('/home/sandbox/app/server.js', `
  require('http').createServer((req, res) => res.end('hello from a microVM'))
    .listen(3000)
`)

// 4. Start a server, explicitly expose its port, and reach it from outside.
sbx.commands.run('node server.js', { timeoutMs: 600_000 }).catch(() => {})
await new Promise((r) => setTimeout(r, 500))
const host = await sbx.exposePort(3000)         // e.g. "100.69.9.101:5203"
console.log(await (await fetch(`http://${host}/`)).text())

// 5. Clean up (or let the TTL reap it)
await sbx.kill()
```

Long-running output streams as it happens:

```ts
await sbx.commands.run('pnpm install', {
  onStdout: (c) => process.stdout.write(c),
  onStderr: (c) => process.stderr.write(c),
  timeoutMs: 120_000,
})
```

Full SDK surface (snapshots, fan-out, ports, errors): [SDK README](../sdk/typescript/README.md).

## Option B: curl

The API is plain JSON over HTTP with a bearer token
([full reference](http-api.md)):

```bash
# Create (blocks until the sandbox is ready; ~0.5s)
curl -s -X POST -H "Authorization: Bearer $SANDBOX_API_KEY" \
  $SANDBOX_API_URL/sandboxes -d '{"timeout_sec": 600}'
# → {"id":"2fdcea66-…","guest_ip":"172.16.0.10",…}

ID=2fdcea66-…   # from the response

# Run a command
curl -s -X POST -H "Authorization: Bearer $SANDBOX_API_KEY" \
  $SANDBOX_API_URL/sandboxes/$ID/exec -d '{"cmd":"node --version"}'
# → {"stdout":"v22.23.1\n","stderr":"","exit_code":0,"timed_out":false,"duration_ms":288}

# Write / read a file
curl -s -X PUT -H "Authorization: Bearer $SANDBOX_API_KEY" \
  --data-binary 'console.log("hi")' \
  "$SANDBOX_API_URL/sandboxes/$ID/files?path=/home/sandbox/hi.js"
curl -s -H "Authorization: Bearer $SANDBOX_API_KEY" \
  "$SANDBOX_API_URL/sandboxes/$ID/files?path=/home/sandbox/hi.js"

# Destroy
curl -s -X DELETE -H "Authorization: Bearer $SANDBOX_API_KEY" \
  $SANDBOX_API_URL/sandboxes/$ID
```

## Option C: CLI (on or near the server)

The `sandbox` binary doubles as a client. On the host itself it talks over the
local Unix socket (root required); from anywhere else, point it at a gateway:

```bash
sudo ./sandbox up                                  # create; prints JSON + URL
sudo ./sandbox list
sudo ./sandbox exec <id> -- "node --version"
sudo ./sandbox shell <id>                          # full interactive terminal
echo 'hello' | sudo ./sandbox write <id> /home/sandbox/hello.txt
sudo ./sandbox read <id> /home/sandbox/hello.txt
sudo ./sandbox down <id>

# Against a fleet gateway (no sudo needed — plain TCP + token):
./sandbox up --gateway 100.69.9.101:9090 --gateway-token $SANDBOX_API_KEY
```

## Where to go next

- Give sandboxes prepared state (deps installed, server warm) and clone it
  32 ways in seconds → [Snapshots & fan-out](concepts.md#snapshots-restore-and-fan-out)
- Expose more ports, set TTLs, understand the lifecycle → [Concepts](concepts.md)
- Run your own host or fleet → [Self-hosting](self-hosting.md)
