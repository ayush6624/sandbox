# Reading the code

A route through ~54k lines of Go you did not write. Most of this codebase was
produced by an AI agent working against `CLAUDE.md`, which changes what a
useful reading strategy looks like: the code is uniformly styled and heavily
commented, so nothing *looks* hard, and importance is not signalled by
prose density. You cannot skim for the tricky part — every part reads tricky.

Six Excalidraw diagrams go with this guide — [`docs/diagrams/`](diagrams/README.md).
Start with `3-decisions` (why it is built this way, with the measurement behind
each call) and `2-clone-timeline` (the sequence that four features share).

Read in **traces, not files**. Six end-to-end paths cover the system; the rest
of the tree is support code you can meet on demand.

## Before anything: `CLAUDE.md` is a spec, not a summary

Read it once, top to bottom, before opening a `.go` file — an hour, badly spent
if rushed. It is the accumulated design memory: every architecture note there
exists because something was measured, broke in production, or was tried and
rejected. When the code and `CLAUDE.md` disagree, that is a finding worth
chasing, not a doc bug to fix silently.

Its notes divide into three kinds, and they age differently:

| Kind | Example | Trust |
| --- | --- | --- |
| Invariant | "the pool is keyed on the SANDBOX, never the guest IP" | High — usually pinned by a test |
| Measurement | "43.1 MiB PSS per VM", "fanout N=16 in 5042 ms" | Dated; re-measure before relying on it |
| Rejected approach | "UFFD: keep it off for small guests" | High — records a negative result you'd otherwise repeat |

## The shape of the thing

Three binaries, one Go module:

| Binary | Runs on | Entry point |
| --- | --- | --- |
| `sandbox serve` | each VM host (worker) | `cmd/sandbox/serve.go` → `internal/server` |
| `sandbox gateway` | the control plane, one per fleet | `cmd/sandbox/gateway.go` → `internal/gateway` |
| `sandboxd` | inside every guest, as PID 1 | `cmd/sandboxd/main.go` |

`sandbox <verb>` (up/down/exec/ssh/…) is the same binary again, as a thin HTTP
client over `internal/client`.

Non-test Go, largest first — this is the honest weight distribution:

```
internal/server/     9.9k   worker: VM lifecycle, snapshots, hibernate, proxy, ports
internal/gateway/    4.9k   placement, routing, queue, autoscale, reverse-proxy
internal/vm/         4.7k   Firecracker SDK + jailer + UFFD
internal/registry/   2.8k   SQLite: pools, state machine, usage ledger
cmd/sandbox/         2.6k   CLI + serve/gateway wiring
cmd/sandboxd/        2.3k   guest agent: exec, files, pty, identity, netlink
internal/apiv1/      1.5k   /v1 contract adapter over the legacy handlers
internal/provisioner/ 1.2k  host networking: bridge, taps, NAT, iptables
```

Everything else (`client`, `config`, `httpapi`, `wsutil`, `agentapi`,
`cluster`, `management`, `metricsapi`, `gcemig`, `gcsblob`) is under 700 lines
each. Read those when a trace walks into them, never before.

Outside Go: `sdk/typescript/` (10k, the real client contract),
`infra/gcp/` (3.7k shell + Nomad HCL, the fleet), `tests/` (TypeScript e2e
suites driven against a live fleet), `docs/` (7.8k markdown).

## Pass 1 — the contract, from outside in (half a day)

Read what a user can ask for before reading how it is served.

1. `docs/concepts.md` — the mental model in prose.
2. `api/openapi.yaml` and `docs/api-v1.md` — the stable surface.
3. `sdk/typescript/src/v1.ts` — how a real client drives it.
4. `internal/server/server.go`, the route table in `Serve` (~line 499). Forty
   routes in one screen: the whole worker API, and the fastest orientation in
   the repo.
5. `internal/apiv1/handler.go` `New`/`Register` — note that `apiv1.New(mux)`
   takes the mux it is about to register *into* as its `legacy` handler. The v1
   layer is an adapter that re-dispatches to the older routes, in both the
   worker (`server.go:543`) and the gateway (`gateway.go:649`). Understanding
   this one line explains why v1 and legacy behaviour can never diverge.

## Pass 2 — the six traces (the actual work)

Follow each one call by call, in order. Function names are the durable anchors;
line numbers drift.

**1. Create, hot path** — the spine of the system.
`server.handleCreate` → `claimWarmForTemplate` (ready pool) → else
`createFromSnapshot` (`golden.go`) → `bringUpClone` → `finishClone`
(`snapshot.go`) → `waitForAgent` (`proxy.go`). Fall-back branch:
`createCold`. Read `golden.go` `ensureGolden`/`buildGolden` first if the golden
snapshot is unfamiliar — every fast path in the repo descends from it.

**2. Exec** — how the host reaches the guest.
`handleAgentProxy` → `ensureRunning` (wakes a hibernated sandbox) →
`agentAuthority`/`dialAgentAuthority` → guest `sandboxd` `/exec`. The
sandbox-keyed connection pool in `proxy.go` is a good example of a
non-obvious invariant with a comment explaining exactly why it exists.

**3. Hibernate and wake** — where the state machine lives.
`hibernateLoop` → `hibernateWithMode` → registry flip to `hibernated`; then
`ensureRunning` → `wakeLocked` → `wakeRestore` (same identity) or the clone
path. Pair this with `registry.go`'s partial unique indexes: taps and IPs are
released on hibernate purely because those indexes only bind `running` rows.

**4. Placement** — the gateway's only real decision.
`gateway.handleCreate` → `reserveHostCreate` → `place` → `landReservation`,
plus `awaitHostDemand` for the queue and `fleetDemand` for autoscaling.

**5. Boot of a worker** — `cmd/sandbox/serve.go`, then `server.Serve`:
`EnsureNetwork` → `reconcile` → `ensureGolden` → warm pool → heartbeat. This
explains the self-healing behaviour that otherwise looks like magic.

**6. The guest side** — `cmd/sandboxd/main.go` `runInit` (PID 1 duties, the
re-exec, the default PATH), then `identity.go` and `garp_linux.go`. Short, and
it closes the loop on why a clone gets a fresh IP.

## Pass 3 — read the tests as the specification

The Go tests are where the invariants are pinned, and they are the best
antidote to plausible-looking generated code. Highest signal per line:

- `internal/registry/registry_test.go` — the sandbox state machine.
- `internal/gateway/gateway_test.go` — placement, failover, queueing.
- `internal/server/agentpool_test.go` — small, and pins the pool-key rule from
  trace 2.
- `internal/apiv1/handler_test.go` — the public contract.
- `tests/` (TypeScript) — needs a live fleet; read `tests/README.md` and the
  suites rather than running them first.

`make build` works on macOS (Firecracker calls stub out to `ErrLinuxOnly`), so
you can compile and run unit tests locally without a Linux host.

## What to skip on a first read

Skip until a trace drags you in: `internal/vm/uffd*` (opt-in, off by default,
and `docs/why-uffd.md` explains it better than the code), `internal/gcsblob`,
`internal/gcemig`, `services/sandbox-edge`, `infra/gcp/*.sh`, and every
`docs/*-plan.md` — plans describe intent at a past moment and several are
partly or wholly superseded by the code.

Also skip `.claude/worktrees/` entirely. It contains full copies of the tree
and will double every grep result you get; scope searches with
`--exclude-dir=.claude`.

## Reading generated code specifically

Four habits that pay for themselves here:

- **Comments are load-bearing but not authoritative.** They were written by the
  same process that wrote the code, at the same moment, so a comment can
  faithfully describe an intention the code does not implement. Treat a comment
  as a claim to check, not a summary to accept.
- **Prefer `git log -S<symbol>` over reading around.** Commit subjects in this
  repo are short and blunt ("Only pay systemctl when an inherited sshd is
  actually running") and usually name the defect. The history is a better
  explanation of *why* than anything in the file.
- **Uniform style hides risk gradients.** `registry.go`'s transactional pool
  allocation and a CLI flag parser look equally considered. Use the traces
  above to decide what deserves care.
- **Suspect duplication before abstraction.** Generated code tends to re-derive
  a helper rather than reuse one. When something looks new, grep for it.

## Checkpoint: can you answer these?

If yes, you can work in this codebase.

1. Why does a create with `mem_mib: 2048` take ~2.5 s while a default create
   takes ~15 ms?
2. What frees a tap device, and why does hibernating a sandbox release it while
   keeping its host port?
3. Where does a `POST /v1/sandboxes` become a `POST /sandboxes`?
4. Why is a 404 from the gateway a stronger statement than a 404 from a worker?
5. Which two processes must agree for a cloned VM to be reachable at its new IP?
