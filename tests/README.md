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

# Optional: snapshot and snapshot-source batch creation are host-local endpoints (the gateway
# doesn't route them yet) — point these at one host directly:
export SANDBOX_HOST_URL=http://<host>:8080
export SANDBOX_HOST_KEY=<host-token>

npm test                       # all suites
npm test -- exec files         # a subset
npm run test:quick             # lifecycle + exec + files
npm run test:stress            # concurrency + churn + load
```

The focused Linux/KVM security gate targets one worker directly so both probe
sandboxes share its bridge:

```bash
SANDBOX_HOST_URL=https://<worker-ip>:8080 \
SANDBOX_HOST_KEY=<host-token> \
WORKER_SSH=you@<worker-ip> \
WORKER_API_HOST=<worker-port-forward-ip-or-hostname> \
./security-gate.sh
```

Set `GUEST_SSH_PROXY_JUMP=<bastion>` when guest port forwards are private and
must be reached through the control-plane SSH host.

It is a fail-closed production-profile gate. It creates two independent
sandboxes plus two snapshot-source sandboxes through the v1 `createMany` batch
contract, then verifies:

- unique non-root VMM users and mount/PID namespaces;
- cgroup v2 placement and finite CPU, memory, PID, file-descriptor, and
  block-I/O controls;
- root-owned per-VM jails, allocation records, and cross-jail denial;
- seccomp, bounded logs, FIFO hygiene, bridge isolation, and allowed egress;
- default guest UID/GID 1000 and denial of guest access to privileged agent
  state/endpoints;
- key-only SSH as `sandbox` with UID 1000 and rejection of remote root login;
- unique SSH host identities for independent and snapshot-source creates, with
  identity preserved through pause/resume;
- reconciliation and jail/cgroup cleanup after one unexpected VMM crash; and
- expected lifecycle cleanup.

Its exit trap deletes only resources created by that invocation. The worker
must use the production jailer profile; direct-development mode fails the gate.
Set `JAILER_BASE` only when the worker intentionally uses a non-default jail
root. `CURL_CA_BUNDLE` can point curl at a private management CA. An `http://`
URL is rejected unless the operator explicitly sets
`PRIVATE_MANAGEMENT_PROXY_ACK=I_VERIFIED_PRIVATE_AUTHENTICATED_PROXY` after
verifying that the listener is reachable only through a private authenticated
proxy.

Server-crash and host-reboot cases are deliberately isolated in a destructive
gate. It requires an empty disposable worker and refuses to start without an
explicit acknowledgement:

```bash
DISPOSABLE_WORKER_RECOVERY=I_UNDERSTAND_THIS_RESTARTS_OR_REBOOTS_A_DISPOSABLE_WORKER \
RECOVERY_MODE=server-crash \
SANDBOX_HOST_URL=https://<disposable-worker>:8080 \
SANDBOX_HOST_KEY=<client-token> \
WORKER_SSH=you@<disposable-worker> \
./security-recovery-gate.sh

# Repeat with RECOVERY_MODE=host-reboot.
```

The recovery gate discovers and validates the exact `sandbox serve` process
before injecting a server crash. The Nomad task supervisor restarts the server
child in place; Nomad's bounded task restart remains the fallback for ordinary
process failures. A normal host reboot may gracefully pause the probe; the gate
accepts either clean deletion or a paused probe whose SSH identity survives
wake. Both modes reject stale jails/cgroups and recheck the bridge firewall
after recovery.

The resource-exhaustion gate is also restricted to an empty disposable worker.
Guest memory, process, and descriptor pressure is bounded inside a 128 MiB
probe, while ENOSPC is induced only on a 16–64 MiB run-owned loop filesystem:

```bash
DISPOSABLE_WORKER_EXHAUSTION=I_UNDERSTAND_THIS_EXHAUSTS_RESOURCES_ON_A_DISPOSABLE_EMPTY_WORKER \
SANDBOX_HOST_URL=https://<disposable-worker>:8080 \
SANDBOX_HOST_KEY=<client-token> \
WORKER_SSH=you@<disposable-worker> \
./security-exhaustion-gate.sh
```

It refuses hosts with less than 2 GiB free on either root or data storage,
checks the control sandbox and API after every pressure phase, and removes only
the invocation's exact sandbox IDs, loop device, mount, and files.

The TypeScript suite exits non-zero on failure and writes its JSON report under
`results/` (gitignored). Both security gates also exit non-zero on a failed
invariant and stream the failing check to stderr.

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
| `snapshots` | snapshot → resume (disk **and** live memory state), snapshot-source batch create N (shared source, isolated writes, unique identities), list/delete housekeeping — *skipped unless `SANDBOX_HOST_URL`/`SANDBOX_HOST_KEY` are set* |
| `clock` | guest wall clock matches host time on hot create (golden snapshot may be hours old) and after hibernate + wake |

## Sizing knobs

All optional, with modest defaults so a full run fits comfortably on one
8-vCPU host:

| Env var | Default | Meaning |
| --- | --- | --- |
| `STRESS_BURST` | 24 | concurrent creates in the burst test |
| `STRESS_LOAD_N` | 16 | sandboxes running the load workload |
| `STRESS_FANOUT_N` | 8 | sandboxes in the snapshot-source batch test (legacy environment variable name) |
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
export TRAFFIC_SCENARIOS="sawtooth-scale-cycle standby-refill-boundary held-burst gradual-ramp second-wave long-lived-reconcile create-exec-kill-churn"

./tests/autoscale-benchmark.sh
```

The wrapper retains its existing `PROJECT`, `ZONE`, `MIG_NAME`,
`GATEWAY_TOKEN`, and `WORKER_SSH_USER` requirements and refuses to start if
the gateway is not empty or the fleet is not at the expected clean floor.
Run a subset by changing `TRAFFIC_SCENARIOS`. The sawtooth defaults to three
bursts and waits up to 22 minutes for scale-in between them (15-minute policy
window plus late-scale-action and reconciliation headroom); tune only via the
documented `AUTOSCALE_*` variables in `autoscale-traffic.ts`.

The acknowledgement and `EXPECTED_WORKER_RELEASE` are required for both the
traffic suite and the legacy held-burst mode. Safety defaults bound a driver to
three hours, cap a legacy burst at 512 creates, cap traffic scenarios at 512
simultaneously live sandboxes and 22 alive hosts, and fail acceptance when
create p95 exceeds 30 seconds or any create exceeds 60 seconds. Override these
only after checking the live `MIG_MAX`, queue wait, slot count, and budget:

| Guard | Default |
| --- | ---: |
| `BENCHMARK_TIMEOUT_SEC` | 10800 |
| `MAX_BURST_COUNT` | 512 |
| `CLEANUP_TIMEOUT_SEC` | 120 |
| `AUTOSCALE_MAX_LIVE_SANDBOXES` | 512 |
| `AUTOSCALE_MAX_HOSTS` | 22 |
| `AUTOSCALE_MAX_CREATE_P95_MS` | 30000 |
| `AUTOSCALE_MAX_CREATE_MS` | 60000 |

Every invocation gets a directory under `tests/results/autoscale-<UTC>/` with
the driver result, combined observer timeline, benchmark log, final fleet
snapshot, run metadata, and SHA-256 checksums. Exit 70 means the driver itself
passed but the wrapper could not prove cleanup. The cleanup trap deletes only
names bearing that invocation's run ID; it never sweeps unrelated sandboxes.
