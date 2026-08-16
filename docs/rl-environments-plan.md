# Running open-source RL environments and agent benchmarks on the sandbox fleet

Goal: pick popular, credible open-source workloads that prove the fleet is a
real substrate for agent evaluation and RL rollouts, and run them end to end
without spending a fortune on tokens.

Everything below is grounded in the upstream code as it stands today (Harbor's
`BaseEnvironment` and `DinDComposeOps`, `verifiers`' v1 `Runtime`), and the
adapters described here are already written under
[`examples/rl-environments/`](../examples/rl-environments/).

---

## 1. The distinction that decides which benchmark to pick

Agent benchmarks split into two kinds, and only one of them is a showcase for
Firecracker microVMs.

**Environment-as-a-machine.** The task *is* a computer: a repo to fix, a broken
service to debug, a shell to drive. Terminal-Bench, SWE-bench, Aider-Polyglot,
CompileBench. These need a real, isolated, disposable machine per attempt, and
the harness's cost and latency are dominated by provisioning it. This is where
we win outright: a Harbor task on EC2 waits ~40 s for an instance and bills an
hour; on our fleet a sandbox is **12 ms p50 / 15 ms p95** from the ready pool
(measured, release `c0d0c0f`, `docs/benchmarks.md`) and is metered per second by
the usage ledger.

**Environment-as-a-process.** The task is a Python simulation with mock tools
and a user simulator: τ³-bench, Zapier's AutomationBench. No microVM is
*required* — the "environment" is in-process tool calls against a fake database.
What these need is hundreds of independent, reproducible, parallel workers, which
we serve well (one prepared snapshot fanned out N ways at **~764 ms per
sandbox**, flat from N=4 up), but the sandbox is a process host rather than the
thing under test. Worth running, and cheap — just don't present it as the proof.

So: **lead with Harbor/Terminal-Bench, and use τ³-bench and AutomationBench as
the cheap breadth-and-scale demo.**

---

## 2. The four use cases, ranked

### UC-1 — Terminal-Bench 2.0 on Harbor, with the `oracle` agent (token cost: $0)

Harbor is the harness from the Terminal-Bench authors (Laude Institute), and it
already has an execution-backend abstraction with Docker, Daytona, Modal, E2B,
Runloop, GKE, EC2 and a dozen more providers. Adding ours is joining a list, not
inventing a category — and `harbor run --environment-import-path` means **no
fork**.

The trick that makes this free: Harbor ships two zero-token agents.
`harbor/agents/oracle.py` runs each task's reference `solve.sh`, and `nop.py`
does nothing at all. Running the full **89-task** `terminal-bench@2.0` dataset
with `--agent oracle` exercises every part of the infrastructure — VM
provisioning, image build, file transfer both ways, exec, the verifier, teardown
— and asserts reward 1.0 on every task, for **zero LLM tokens**. `nop` asserts
the complement: reward 0, i.e. the tests really do fail when nothing happens, so
a green oracle run isn't a harness that always passes.

This is the highest-value single thing to build. It is a *correctness gate for
our own product*, expressed in someone else's widely-trusted task suite.

Datasets available in Harbor's registry, sized for a first run:
`hello-world@1.0` (1 task), `terminal-bench-sample@2.0` (10),
`compilebench@1.0` (15), `mlgym-bench@1.0` (12), `otel-bench@1.0` (26),
`terminal-bench@2.0` (89), `terminal-bench-pro@1.0` (200).

### UC-2 — the same suite with a real agent (token cost: tens of dollars)

Once oracle is green, swap `--agent oracle` for a real one — `mini-swe-agent`
(deliberately minimal scaffold, so the fewest tokens per task) or `claude-code`
— on `terminal-bench-sample@2.0` first, then the full 89. Now the numbers are
about the *agent*, but the interesting artifact for us is the operational one:
89 tasks × k trials of concurrent microVM churn, with per-task VM time from the
usage ledger.

### UC-3 — RL rollouts through `verifiers` (token cost: whatever the sampler costs)

Prime Intellect's `verifiers` is the library behind the Environments Hub,
`prime-rl`, and AutomationBench itself. Its v1 harnesses run rollout programs
through a pluggable `Runtime`; the shipped ones are `subprocess`, `docker`,
`modal`, `prime`. Ours becomes the fifth.

Three fleet features map directly onto how a rollout actually behaves:

- A rollout alternates *policy thinking* (seconds to minutes, no guest work)
  with *environment stepping* (milliseconds). `pause`/`resume` bills nothing for
  the frozen span, and wake is ~80 ms warm.
- Per-task state reset is `snapshot` + create-from-snapshot: prepare a task's
  world once, clone it per rollout. A GRPO-style group of 8 or 16 rollouts over
  identical initial state is exactly the fan-out primitive we already have.
- `GET /v1/usage` makes cost-per-rollout a number rather than a guess.

Unlike Harbor, `verifiers` has **no plugin escape hatch** — `RuntimeConfig` is a
closed discriminated union — so this one needs a small patch to
`verifiers/v1/runtimes/__init__.py` (two type-union lines and a branch), which is
also the natural shape of an upstream PR. The patch is spelled out at the top of
[`verifiers_sandbox_runtime.py`](../examples/rl-environments/verifiers_sandbox_runtime.py).

### UC-4 — token-cheap breadth: τ³-bench and AutomationBench sharded across the fleet

τ³-bench is Sierra's current tool-agent-user benchmark (retail, airline, banking
knowledge, telecom); AutomationBench is Zapier's 600-task business-workflow
benchmark built *on* `verifiers` and published to the Environments Hub — the
Zapier work referenced in the original ask. Both are tool-calling dialogues, so
per-task token cost is modest and controllable with `--limit`.

Neither needs a VM, so the demo here is scale and reproducibility: install once,
snapshot, clone N ways, run shard *i* in clone *i*, merge. That is
[`shard-runner.ts`](../examples/rl-environments/shard-runner.ts), and its
`--dry-run` mode runs the whole harness with a scripted no-LLM policy for **zero
tokens** — a useful fan-out load test in its own right.

---

## 3. Recommendation

Build **UC-1 first** (Harbor environment + oracle sweep), because it is the only
one that is simultaneously a marketing artifact, a regression gate, and free.
Then **UC-4** (it needs no new adapter beyond what's written, and gives a
1000-sandbox-class scale story cheaply), then **UC-3** for the RL narrative, and
**UC-2** whenever there's token budget to spend on agent numbers rather than
infrastructure numbers.

---

## 4. How the Harbor adapter works

Harbor tasks are compose projects: a `task.toml` plus a `docker-compose.yaml` or
`Dockerfile`, with `[environment] cpus / memory_mb / storage_mb`. Two provider
strategies exist upstream:

1. **Provider builds the image** — `E2BEnvironment` turns the task's Dockerfile
   into an E2B template; Daytona builds a Daytona snapshot. We *can't* do this
   today: `/v1/templates` is read-only, there is no template-build API.
2. **Docker-in-the-VM** — `EC2Environment` provisions a VM, installs Docker, and
   runs `docker compose build && up` inside it, execing into the `main` service.
   `DinDComposeOps` factors out everything except two primitives: run a shell
   command on the VM, and move files to/from it.

We take route 2. `SandboxEnvironment` is ~380 lines because `DinDComposeOps`
does the rest, and it maps the task's `cpus`/`memory_mb` onto the **guest VM**
rather than a cgroup inside it — a stronger boundary than Docker limits, and
honest about CPU (the fleet oversubscribes CPU ~6:1 by design, so we declare
`cpu_request` but not `cpu_limit`).

```
harbor run --environment-import-path …:SandboxEnvironment
        │
        ├─ preflight()   → SANDBOX_API_URL / SANDBOX_API_KEY present
        ├─ start()       → POST /v1/sandboxes            (~12 ms, ready pool)
        │                  wait for dockerd              (baked, already up)
        │                  upload compose overlays + task env dir
        │                  docker compose build && up -d   (inside the guest)
        ├─ exec()        → docker compose exec -T main bash -lc …
        ├─ upload/download → two-hop: API → guest /tmp → docker compose cp
        └─ stop(delete)  → DELETE /v1/sandboxes/{id}     (kills the whole project)
```

**Prerequisite: Docker must be baked into the base rootfs.** The image ships
Node/pnpm/TypeScript/Python/build-essential/git but no Docker, and installing it
per task would cost 30–60 s of the task's own budget and need egress.
[`bake-docker-into-rootfs.sh`](../examples/rl-environments/bake-docker-into-rootfs.sh)
loop-mounts the rootfs and installs `docker.io` + `docker-compose-v2`, optionally
pre-pulling task base images so a task's first compose build hits no network at
all. The 10 GiB sparse rootfs has ample room.

Two consequences worth stating plainly, both already documented in CLAUDE.md and
both intended:

- A rootfs change bumps the base mtime, which **invalidates the golden snapshot**
  (`goldenUsable` keys on base mtime+size). Restart `serve` and it cold-builds a
  fresh golden that includes Docker. On the fleet this is a rebake
  (`bake-image.sh bake && golden`) plus a MIG roll, not a `rollout.sh` deploy.
- Docker-in-microVM is containers inside a VM, not nested virtualization. The
  workers already run with nested virt enabled for KVM; this needs nothing extra.

---

## 5. Build plan

Each phase has an explicit gate. Don't start the next one until the gate is
green — the whole point of the oracle agent is that the gates are cheap.

| Phase | Work | Gate |
|---|---|---|
| **0. Docker in the guest** | Run `bake-docker-into-rootfs.sh` on one dev host; restart `serve`; confirm the golden rebuilt | `sandbox exec <id> -- "docker run --rm hello-world"` succeeds; create latency back to the hot path |
| **1. Adapter smoke** | `harbor run --dataset hello-world@1.0 --agent oracle --environment-import-path …` | 1/1 task, reward 1.0 |
| **2. Negative control** | Same, `--agent nop` | 1/1 task, reward 0.0 — proves the verifier actually verifies |
| **3. Small sweep** | `terminal-bench-sample@2.0` (10 tasks), `--n-concurrent 4`, oracle | 10/10; no leaked sandboxes in `sandbox list` |
| **4. Full oracle sweep** | `terminal-bench@2.0` (89), `--n-concurrent 32` | ≥ 85/89 (a handful of upstream tasks are flaky by nature); every failure attributed to a task, not to us; `GET /v1/usage` reports the run |
| **5. Fleet scale** | Raise `--n-concurrent` until the gateway queues, then 503s | Capacity errors are 503 + Retry-After (never 500/404); the autoscaler adds hosts; nothing leaks after teardown |
| **6. Real agent** | `mini-swe-agent` on the 10-task sample, then 89 with `--n-concurrent 16` | Comparable pass rate to the published leaderboard for that model |
| **7. Cheap breadth** | `shard-runner.ts --dry-run --shards 32`, then `tau3 --shards 8 --limit 32` | 32/32 shards; fan-out ≈ 764 ms/sandbox; results merge |
| **8. verifiers runtime** | Patch the runtime union; run one `verifiers` env end-to-end; then a snapshot-per-task group fan-out | A rollout completes; `pause_between: true` shows near-zero billed time during sampling |

Phases 0–5 cost **no tokens at all**. That is the point.

---

## 6. What it costs

### Sandbox time

A Terminal-Bench task is minutes of VM time. 89 tasks × ~4 min ≈ 6 VM-hours per
oracle sweep; at 32-way concurrency that's ~11 minutes of wall clock. On
`n2-standard-16` workers with `SLOTS_PER_HOST=48` and 2 GiB task VMs
(2048 + 156 MiB overhead ≈ 1.87 slots each), one worker holds ~25 concurrent
tasks, so a 32-way sweep spans two workers — well inside `MIG_MAX=22`.

### Tokens (UC-2 / UC-4 only)

Don't take an estimate on faith — **calibrate**. Run 5 tasks, read
`usage.input_tokens` / `output_tokens` / `cache_read_input_tokens` off the
traces, then multiply. As illustrative arithmetic for one Terminal-Bench task
with a minimal-scaffold agent (~40 turns, ~300 K cached-read input, ~60 K fresh
input, ~30 K output):

| Policy model | Model ID | Price (in/out per MTok) | ≈ per task | ≈ 89 tasks |
|---|---|---|---|---|
| Claude Haiku 4.5 | `claude-haiku-4-5` | $1 / $5 | ~$0.24 | ~$21 |
| Claude Sonnet 5 | `claude-sonnet-5` | $3 / $15 — **$2 / $10 intro through 2026-08-31** | ~$0.48 at intro pricing | ~$43 |
| Claude Opus 5 | `claude-opus-5` | $5 / $25 | ~$1.05 | ~$93 |

Start on Haiku 4.5. It is the cheapest way to get a *non-zero, comparable*
number, and for infrastructure purposes the pass rate barely matters — what
matters is that 89 concurrent agent sessions drove 89 microVMs correctly.

Three token-cost levers that actually move the needle:

- **Prompt caching.** Cache reads are ~0.1× input price, writes 1.25× (5-minute
  TTL). An agent loop resends a growing prefix every turn, so this is the
  difference between $21 and something several times that. Two gotchas: the
  minimum cacheable prefix is **4096 tokens on Haiku 4.5** but 1024 on Sonnet 5,
  so a short system prompt silently won't cache on Haiku; and any per-request
  byte in the prefix (a timestamp, an unsorted JSON dump) invalidates everything
  after it. Verify with `usage.cache_read_input_tokens` — if it's zero across
  turns, something is invalidating.
- **`--limit` / small datasets.** `terminal-bench-sample@2.0` is 10 tasks and
  answers most infrastructure questions.
- **Haiku 4.5 is a pre-4.6 model.** If you write your own agent loop rather than
  using Harbor's: thinking on Haiku 4.5 is `thinking: {type: "enabled",
  budget_tokens: N}`, and `output_config.effort` **errors** on it. Sonnet 5 is
  the opposite — adaptive thinking is on by default, `budget_tokens` is rejected,
  and non-default `temperature`/`top_p`/`top_k` are rejected too.

---

## 7. Gaps and risks, stated up front

| Gap | Consequence | Mitigation |
|---|---|---|
| No Docker in the base image | Harbor tasks can't run at all | Phase 0 bake; it's a one-time image change |
| No template-build API (`/v1/templates` is read-only) | We can't take the E2B route of turning a task Dockerfile into a native template; every task pays a compose build inside the guest | Pre-pull common base images into the rootfs. Longer term, a template-build API would let a task's image become a snapshot and cut task setup to a clone |
| No per-sandbox egress policy | Harbor tasks that require network isolation, and `verifiers` configs with `allow`/`block` lists, are refused | Declared as unsupported capabilities so both harnesses refuse up front rather than running unisolated. Real fix: per-sandbox iptables policy on the worker |
| Buffered exec caps output at 2 MiB/stream | A verbose 40-minute agent run would truncate | Both adapters use streaming exec / redirect to a file and download |
| Idle hibernation vs a thinking policy | A policy thinking for two minutes looks exactly like an idle sandbox; an unexpected freeze shows up as one mysteriously slow step | Both adapters set `idle_timeout_seconds: -1` and pause *explicitly* instead |
| Fan-out past free capacity | 503 + Retry-After | `acquire_with_backoff()` retries only capacity errors; batch create is bounded server-side. Never retry a non-capacity error — that's a real failure |
| Upstream task flakiness | A red run that isn't our fault | The `nop` control run and per-task attribution; compare against the published leaderboard for the same agent+model |

---

## 8. What already exists in this repo

| File | What it is |
|---|---|
| [`examples/rl-environments/sandbox_client.py`](../examples/rl-environments/sandbox_client.py) | Async Python client for the v1 API (lifecycle, exec, files, tar dir transfer, snapshots, port forwards, usage). The RL ecosystem is Python and we only ship a TS SDK, so this is the bridge. Depends only on `httpx` |
| [`harbor_sandbox_env.py`](../examples/rl-environments/harbor_sandbox_env.py) | `SandboxEnvironment(ComposeServiceOpsMixin, BaseEnvironment)` — the Harbor backend, loadable via `--environment-import-path`, no fork |
| [`verifiers_sandbox_runtime.py`](../examples/rl-environments/verifiers_sandbox_runtime.py) | `SandboxRuntime(Runtime)` for `verifiers` v1, plus `prepare_task_snapshot()` / `fanout_rollouts()` helpers and the exact union patch needed to register it |
| [`shard-runner.ts`](../examples/rl-environments/shard-runner.ts) | Prepare-once/clone-N-ways shard runner over the TS SDK, with a zero-token `--dry-run` mode |
| [`bake-docker-into-rootfs.sh`](../examples/rl-environments/bake-docker-into-rootfs.sh) | Bakes Docker (and optional pre-pulled images) into the base rootfs |

All Python syntax-compiles, the TypeScript typechecks against the SDK, and the
shell script passes `bash -n`. **None of it has been run against a live fleet
yet** — that is phase 0.

---

## Sources

- [Introducing Terminal-Bench 2.0 and Harbor](https://www.tbench.ai/news/announcement-2-0)
- [Harbor documentation](https://harbor-framework-harbor.mintlify.app/introduction) · [`harbor-framework/harbor`](https://github.com/harbor-framework/harbor)
- [PrimeIntellect-ai/verifiers](https://github.com/PrimeIntellect-ai/verifiers) · [Environments Hub](https://www.primeintellect.ai/blog/environments) · [prime-rl](https://github.com/PrimeIntellect-ai/prime-rl)
- [zapier/AutomationBench](https://github.com/zapier/AutomationBench) · [How Zapier turned AutomationBench into a continuous agent improvement loop](https://www.primeintellect.ai/case-study/zapier)
- [sierra-research/tau2-bench](https://github.com/sierra-research/tau2-bench)
- Fleet numbers: [`docs/benchmarks.md`](benchmarks.md), release `c0d0c0f`
