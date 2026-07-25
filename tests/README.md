# tests — end-to-end stress & correctness suite

Exercises a **live** sandbox deployment (single host or fleet gateway) through
the TypeScript SDK (`sdk/typescript`, imported from source). No mocks — every
test creates real microVMs, so point it at a test fleet, not something you
care about.

Every sandbox created through the harness is tracked and killed at the end of
its suite (pass, fail, or Ctrl-C), and the churn suite explicitly asserts the
fleet returns to its baseline sandbox count.

## Run

```bash
cd tests
npm install

export SANDBOX_API_URL=http://<gateway-or-host>:<port>
export SANDBOX_API_KEY=<token>

# Optional: snapshot/restore/fanout are host-local endpoints (the gateway
# doesn't route them yet) — point these at one host directly:
export SANDBOX_HOST_URL=http://<host>:8080
export SANDBOX_HOST_KEY=<host-token>

npm test                       # all suites
npm test -- exec files         # a subset
npm run test:quick             # lifecycle + exec + files
npm run test:stress            # concurrency + churn + load
```

Exit code is non-zero on any failure; a JSON report lands in `results/`
(gitignored).

`diag.ts` is a standalone fleet diagnostic (guest DNS, pnpm shim, `/home/sandbox`
layout) handy when a run fails in odd ways: `npx tsx diag.ts`.

**Note on flaky multi-minute stalls:** when driving the fleet over Tailscale
from far away, the path can stall for minutes under sustained load (requests
report client-side timeouts long after the server finished the work, plus
`fetch failed` collateral). If a run shows several unrelated timeouts at once,
suspect the network path first and re-run before blaming the fleet.

## Suites

| Suite | What it covers |
| --- | --- |
| `lifecycle` | create/connect/list/kill, error mapping (404/401), TTL reaper, `setTimeout` extend + clear |
| `exec` | exit codes, `CommandExitError`, envs/cwd, unicode, timeout → process-group kill, backgrounding, 2 MiB output cap, streaming parity with buffered exec, 50 sequential execs |
| `files` | text/binary/8 MiB round-trips (sha256-verified host- and guest-side), parent-dir creation, listing, overwrites, `NotFoundError`, 20 concurrent writes |
| `ports` | reaching guest servers from outside via `getHost`, `exposePort` end-to-end + idempotency, `listPorts`, cross-sandbox isolation, forward teardown on kill, wake-on-connect for hibernated sandboxes |
| `concurrency` | burst creates (unique ids/IPs/ports, placement spread, all usable), 16 parallel execs on one agent, mixed parallel API load, create-during-kill overlap |
| `churn` | sequential + batched create→exec→kill cycles, immediate reuse, **leak check: fleet returns to baseline count** |
| `load` | N sandboxes running a verified CPU+disk workload concurrently, memory pressure, many-small-files disk churn |
| `snapshots` | snapshot → restore (disk **and** live memory state resume), fanout N (shared state, isolated writes, unique identities), list/delete housekeeping — *skipped unless `SANDBOX_HOST_URL`/`SANDBOX_HOST_KEY` are set* |
| `clock` | guest wall clock matches host time on hot create (golden snapshot may be hours old) and after hibernate + wake |

## Sizing knobs

All optional, with modest defaults so a full run fits comfortably on one
8-vCPU host:

| Env var | Default | Meaning |
| --- | --- | --- |
| `STRESS_BURST` | 24 | concurrent creates in the burst test |
| `STRESS_LOAD_N` | 16 | sandboxes running the load workload |
| `STRESS_FANOUT_N` | 8 | clones in the fanout test |
| `STRESS_CHURN_CYCLES` | 8 | sequential churn cycles |
| `STRESS_CHURN_ROUNDS` / `STRESS_CHURN_BATCH` | 3 / 6 | batched churn rounds × size |

Crank them up for a real stress run, e.g.:

```bash
STRESS_BURST=64 STRESS_LOAD_N=48 STRESS_FANOUT_N=32 npm test -- concurrency load snapshots
```

## Destructive autoscaling traffic suite

`autoscale-traffic.ts` is a separate live-fleet benchmark. It covers a held
burst beyond the warm floor, gradual ramp, a second burst while the first
scale-out is in progress, long-lived sandboxes during worker reconciliation,
create/exec/kill churn, repeated scale-out/scale-in sawteeth, and a
`standby-refill-boundary` regression that holds live sandboxes across the
standby pool's initial-delay boundary while proving newly created refill
workers remain placement-quarantined until safely suspended. Successful
creates are repeatedly reconnected and executed against; host release,
capacity, placement, routing, and final cleanup invariants are monitored
independently.

Run it from the control VM through `autoscale-benchmark.sh` so the existing
GCE, SSH, Nomad, gateway, and queue timeline is captured on the same clock:

```bash
export EXPECTED_WORKER_RELEASE=<deployed-release>
export LIVE_AUTOSCALE_BENCHMARK=I_UNDERSTAND_THIS_CREATES_REAL_VMS
export TRAFFIC_SCENARIOS="sawtooth-scale-cycle held-burst gradual-ramp second-wave long-lived-reconcile create-exec-kill-churn"

./tests/autoscale-benchmark.sh
```

The wrapper retains its existing `PROJECT`, `ZONE`, `MIG_NAME`,
`GATEWAY_TOKEN`, and `WORKER_SSH_USER` requirements and refuses to start if
the gateway is not empty or the fleet is not at the expected clean floor.
Run a subset by changing `TRAFFIC_SCENARIOS`. The sawtooth defaults to three
bursts and waits up to 22 minutes for scale-in between them (15-minute policy
window plus late-scale-action and reconciliation headroom); tune only via the
documented `AUTOSCALE_*` variables in `autoscale-traffic.ts`.
