# Running open-source RL environments and agent benchmarks on the fleet

Adapters that let two upstream harnesses use our Firecracker sandboxes as their
execution backend, plus a shard runner for benchmarks that don't need a VM at
all. The reasoning, phased build plan, cost model, and known gaps are in
[`docs/rl-environments-plan.md`](../../docs/rl-environments-plan.md).

| File | What it is |
|---|---|
| `sandbox_client.py` | Async Python client for the v1 API. The RL ecosystem is Python; we ship a TS SDK. Only dependency is `httpx` |
| `harbor_sandbox_env.py` | Harbor execution backend (Terminal-Bench 2.0 and ~80 other datasets). No fork needed |
| `verifiers_sandbox_runtime.py` | `verifiers` v1 runtime (Prime Intellect: Environments Hub, prime-rl, AutomationBench). Needs a two-line union patch — spelled out in the module docstring |
| `shard-runner.ts` | Prepare-once / clone-N-ways benchmark sharding over the TS SDK, with a zero-token `--dry-run` mode |
| `bake-docker-into-rootfs.sh` | Bakes Docker into the base rootfs — a prerequisite for Harbor tasks |

Nothing here has been run against a live fleet yet. Start at phase 0 of the plan.

## Prerequisite: Docker in the guest

Harbor tasks are `docker-compose` projects, so the guest has to be a Docker host.
The base image doesn't ship Docker, and installing it per task would cost 30–60 s
of the task's own budget.

```bash
# on a sandbox host, as root
sudo bash examples/rl-environments/bake-docker-into-rootfs.sh \
    --pull ubuntu:24.04 --pull python:3.13-slim
sudo ./sandbox stop-server
sudo ./sandbox serve --config configs/devbox.json    # rebuilds the golden snapshot
```

This bumps the base rootfs mtime, which invalidates the golden snapshot by
design — the restart cold-builds a new one that includes Docker. On the GCP fleet
it's a rebake (`infra/gcp/bake-image.sh bake && golden`) plus a MIG roll, not a
`rollout.sh` deploy.

Verify before going further:

```bash
sudo ./sandbox up
sudo ./sandbox exec <id> -- "docker run --rm hello-world"
```

## Harbor / Terminal-Bench

```bash
pip install harbor httpx
export SANDBOX_API_URL=http://10.160.0.100:9090   # gateway; run from the control VM
export SANDBOX_API_KEY=...

# 1 task, zero tokens — the oracle agent runs the task's reference solution
harbor run --dataset hello-world@1.0 --agent oracle \
  --environment-import-path examples.rl_environments.harbor_sandbox_env:SandboxEnvironment

# negative control: nop must score 0, or the verifier isn't verifying
harbor run --dataset hello-world@1.0 --agent nop \
  --environment-import-path examples.rl_environments.harbor_sandbox_env:SandboxEnvironment

# the real thing: 89 tasks, 32-way concurrency, still zero tokens
harbor run --dataset terminal-bench@2.0 --agent oracle --n-concurrent 32 \
  --environment-import-path examples.rl_environments.harbor_sandbox_env:SandboxEnvironment

# with an actual agent (this one costs money)
harbor run --dataset terminal-bench-sample@2.0 --agent mini-swe-agent \
  --model claude-haiku-4-5 --n-concurrent 4 \
  --environment-import-path examples.rl_environments.harbor_sandbox_env:SandboxEnvironment
```

The import path is a module path, so run from the repo root with it importable
(`PYTHONPATH=.`), and note `rl_environments` — Python can't import a directory
with a hyphen, so either add an `__init__.py`-bearing symlink or install the
adapter as a small package. The simplest thing that works:

```bash
ln -s rl-environments examples/rl_environments
touch examples/__init__.py examples/rl_environments/__init__.py
PYTHONPATH=. harbor run ...
```

## verifiers

`verifiers` has no import-path escape hatch — `RuntimeConfig` is a closed
discriminated union — so registering the runtime is a small patch to
`verifiers/v1/runtimes/__init__.py`. The exact diff is in the
`verifiers_sandbox_runtime.py` docstring. Once registered, select it with
`{"type": "sandbox"}` in a runtime config.

Smoke-test the runtime standalone first:

```bash
PYTHONPATH=. python -m examples.rl_environments.verifiers_sandbox_runtime
```

Two helpers worth using directly for RL work:

```python
snapshot_id = await prepare_task_snapshot(client, setup_script)   # pay setup once
ids = await fanout_rollouts(client, snapshot_id, count=16)        # a rollout group
```

## Cheap breadth: τ³-bench and AutomationBench

```bash
cd tests && npm install && cd ..     # tsx + the SDK's dev deps

# zero-token fan-out load test: 32 shards, scripted policy
npx tsx examples/rl-environments/shard-runner.ts --dry-run --shards 32

# real run, cheap model, 32 tasks
export ANTHROPIC_API_KEY=...
npx tsx examples/rl-environments/shard-runner.ts tau3 \
  --shards 8 --limit 32 --model claude-haiku-4-5
```

The CLI flags each benchmark expects (`--shard`, `--num-shards`, `--output`) are
best-guess against upstream and are the first thing to check if a shard fails
immediately — fix them in the `Benchmark` definition at the top of the file.

Drive all of this **from the control VM**, not a laptop tunnel: hundreds of ms of
transport RTT reads as sandbox-creation latency.
