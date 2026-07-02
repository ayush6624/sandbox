# Concepts

How the sandbox system works, and the mental model for using it well.

## What a sandbox is

A sandbox is a Firecracker microVM: its own kernel, filesystem, and network
stack, isolated by KVM hardware virtualization (the same technology behind AWS
Lambda). It boots Ubuntu 24.04 with Node 22, pnpm, TypeScript, Python 3, git,
and build-essential. Nothing else runs by default — it's a blank dev machine.

Inside every sandbox runs `sandboxd`, a small agent the server proxies to for
command execution, file I/O, and interactive shells. **Create only returns once
this agent answers**, so a sandbox you just created is immediately usable — no
polling, no "wait for ready" loop.

| Property | Value |
| --- | --- |
| Guest OS | Ubuntu 24.04 (Node 22, Python 3, git, build tools) |
| Resources | 2 vCPUs, 1 GiB RAM per sandbox (template-wide, configurable) |
| Default working dir | `/home/sandbox/app` |
| Commands run as | root, via `bash -lc` |
| Output cap | 2 MiB each for stdout/stderr per exec (streaming exec included) |
| Default exec timeout | 60 s (override per call; process group is killed on timeout) |

## Why creates are fast (hot create)

You don't have to do anything to get fast creates. At startup, each host boots
one pristine sandbox, snapshots it (memory + disk), and destroys it. Every
`POST /sandboxes` then **clones that "golden snapshot"** instead of cold
booting: the clone resumes from the saved memory image with the agent already
running, gets a fresh IP/MAC/ports, and announces itself the instant its
network identity is applied.

- Hot create: **~200-500 ms** end to end.
- Fallback: if no golden snapshot exists yet (first minutes after a fresh
  install, or when disabled via config), creates cold-boot in ~2-3.5 s.
  Same API, same result — just slower.

The golden snapshot is invalidated automatically when the base image changes
(e.g. after `install-agent`) and rebuilt on the next server restart.

## Snapshots, restore, and fan-out

A snapshot captures a **running** sandbox completely: memory, running
processes, open sockets, and disk. It's not an image build — it's a moment
frozen in time. Restore brings that moment back in a few hundred milliseconds.

The workflow that makes this powerful:

```
create → install deps, warm caches, start server   (slow, once)
       → snapshot                                   (~1s pause, source keeps running)
       → kill source
       → restore / fanout(N)                        (fast, many times)
```

Two ways to bring a snapshot back:

- **Restore** (`POST /snapshots/{id}/restore`) — a 1:1 resurrection. The new
  sandbox reuses the network identity baked into the snapshot, so at most one
  restore of a given snapshot runs at a time, and the source must be dead.
- **Fan-out** (`POST /snapshots/{id}/fanout {"count": N}`) — N independent
  clones at once. Each clone gets a fresh IP/ports and a copy-on-write disk,
  then sheds the snapshot's baked identity before joining the network (it
  announces the swap with a gratuitous ARP; the host holds the clone off the
  bridge until then). Clones share the snapshot's memory/disk state but their
  writes are fully isolated. Measured: 32 clones live in ~2.7 s.

Fan-out is what you want for "run this test suite 32 ways", "give every agent
in a swarm the same prepared repo", or "A/B a change against identical state".

Note: snapshots are per-host. Through a fleet gateway, taking a snapshot works
(it's routed to the sandbox's host), but restore/fan-out/list/delete must be
called on the owning host directly. Gateway routing for snapshots is roadmap.

## Lifecycle and TTLs

Sandboxes live until killed — unless you give them a timeout:

- `POST /sandboxes {"timeout_sec": 600}` — auto-destroy in 10 minutes.
- `POST /sandboxes/{id}/timeout {"timeout_sec": N}` — replace the TTL,
  counting from now. `0` clears it.
- A reaper destroys expired sandboxes within ~10 s of their deadline.

Always set a TTL for programmatic use. It's your safety net against leaked
VMs when a client crashes between `create` and `kill`.

Destroy (`DELETE /sandboxes/{id}`) attempts a graceful in-guest shutdown, then
releases everything: the VM process, its tap device, IP, host ports, and disk.

If the **server** restarts, all sandboxes die with it (VMs live in-process).
On startup the server reconciles: orphaned VM processes are killed and all
leftover resources are reclaimed. Snapshots survive restarts; running
sandboxes don't — treat sandboxes as disposable and snapshots as durable.

## Ports and networking

Each sandbox gets a private IP on a host-internal bridge. Three directions:

- **Guest → Internet** works out of the box (NAT). `pnpm install`, `git
  clone`, `curl` — all fine.
- **You → Guest**: guest port **3000** is pre-forwarded to a dedicated host
  port at create time (`host_port` in the create response; `getHost(3000)` in
  the SDK). Start anything on `:3000` inside and it's reachable at
  `<api-host>:<host_port>`.
- **More ports on demand**: `POST /sandboxes/{id}/ports {"guest_port": 8000}`
  forwards another guest port (idempotent; returns the host port).

Sandboxes on the same host can also reach each other over the bridge — handy
for multi-service setups, but remember they are only as isolated from each
other as your guests' own listening services.

## Multi-host (fleet mode)

One server = one host's worth of sandboxes (64 slots by default). To scale
beyond that, run a **gateway** in front of N hosts:

```
            ┌──────────┐  heartbeat (5s)   ┌────────────┐
  clients → │ gateway  │ ←──────────────── │ host 1..N  │ → microVMs
            │  :9090   │ ──proxy by id───→ │  :8080     │
            └──────────┘                   └────────────┘
```

- Hosts register and heartbeat `{addr, slots, sandbox_ids}`; the gateway holds
  no durable state and rebuilds its routing after a restart.
- `POST /sandboxes` places on the least-loaded live host; everything
  id-scoped (`exec`, files, `shell`, snapshot) is transparently proxied to the
  owning host. `GET /sandboxes` merges across hosts.
- Clients use the **same API** against the gateway — same SDK, same token
  pattern. You generally shouldn't care which host a sandbox landed on.

`GET /hosts` on the gateway shows fleet state (per-host slots, liveness).

## Performance reference

Measured on the 4-host GCP fleet (n2-standard-8, XFS reflink storage):

| Operation | Latency |
| --- | --- |
| Create (hot, via gateway) | ~0.5 s end-to-end (~200-270 ms server-side) |
| Create (cold-boot fallback) | ~2-3.5 s |
| Restore from snapshot | ~0.2-0.3 s server-side |
| Fan-out 32 clones | ~2.7 s total (~85 ms/clone amortized) |
| Snapshot a running sandbox | ~1 s (source pauses briefly, keeps running) |
| Exec round-trip | tens of ms + your command |

## Isolation model

| | Firecracker sandbox | Docker container |
| --- | --- | --- |
| Kernel | dedicated per sandbox | shared with host |
| Isolation boundary | hardware (KVM) | kernel namespaces |
| Device surface | minimal (no USB/GPU/PCI) | full host kernel syscall surface |
| Untrusted code | designed for it | requires extra hardening |

Run untrusted, LLM-generated, or user-submitted code with VM-level isolation
at container-like latency. The remaining shared surface is the sandbox
server's HTTP API itself — keep tokens secret and don't expose listeners
publicly (see [Self-hosting](self-hosting.md#network-exposure)).
