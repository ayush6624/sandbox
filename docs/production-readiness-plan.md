# Production readiness and public API plan

Status: **active**. This is the implementation plan for taking the current
single-operator runtime to a production-quality sandbox service while replacing
implementation terms such as "fan-out" with standard resource and lifecycle
abstractions.

This plan deliberately does **not** include the previously proposed P0 item for
per-tenant authorization, quotas, or API rate/concurrency limits. Those remain
necessary before offering a shared public service, but are deferred from this
workstream by product decision.

## Current baseline

The runtime already has a substantial foundation:

- Firecracker microVM lifecycle, hot creation, snapshots, hibernation, ports,
  command/file/shell access, and a TypeScript SDK.
- A stateless fleet gateway with placement, queueing, autoscaling signals,
  worker release gating, snapshot routing, and cross-host hibernation adoption.
- Crash reconciliation, memory-aware admission, snapshot durability through
  GCS, Prometheus metrics, stress suites, and measured fleet benchmarks.
- Passing Go and TypeScript unit/integration-style tests on the development
  host when local test sockets are available.

The largest remaining risks are the host isolation boundary, the public API
contract, and operational failure handling.

## Public terminology

Public concepts should describe the outcome a user wants, not the mechanism
used to implement it.

| Current term | Public replacement | Contract |
| --- | --- | --- |
| fan-out | batch create / `createMany` | Create N independent sandboxes from the same immutable source |
| fan-out clone | sandbox created from a snapshot | A normal sandbox whose source is a snapshot |
| restore (1:1) | resume, only for the same sandbox | Continue an existing paused sandbox without changing its identity |
| hibernate | pause / resume | User lifecycle; hibernation remains an internal storage mechanism |
| golden snapshot | template revision / base image | Service-managed starting state, hidden from user snapshots |
| timeout | TTL / lifetime | Time until permanent deletion |
| hibernate-after | idle timeout | External inactivity before automatic pause |
| kill | terminate / delete | Permanently destroy a sandbox |
| host port | port forward | Explicit guest-port mapping |

This follows the dominant model used by E2B, Daytona, and Modal: create a
sandbox with a template/image/snapshot as its source. A snapshot is reusable
one-to-many input. Pause/resume is the one-to-one lifecycle operation. "Fan-out"
can remain in benchmark and implementation discussions, but not as the primary
public method.

Research references:

- [E2B snapshots](https://e2b.dev/docs/sandbox/snapshots)
- [Daytona snapshots](https://www.daytona.io/docs/snapshots/)
- [Modal sandbox snapshots](https://modal.com/docs/guide/sandbox-snapshots)
- [Fly Machine lifecycle](https://fly.io/docs/machines/api/machines-resource/)
- [Firecracker production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [OpenAPI Specification](https://spec.openapis.org/oas/)
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html)
- [Google AIP-151: long-running operations](https://google.aip.dev/151)
- [Google AIP-158: pagination](https://google.aip.dev/158)
- [Google AIP-185: API versioning](https://google.aip.dev/185)
- [Stripe idempotent requests](https://docs.stripe.com/api/idempotent_requests)

## P0: runtime and host security

P0 is complete only when all applicable launch paths—cold boot, hot clone,
snapshot restore, hibernation wake, and UFFD restore—provide the same security
properties. A partial mode must not be labelled production-secure.

### P0.1 Firecracker seccomp

Status: **implemented and fleet-verified for enabled production paths**.

- Keep Firecracker's built-in restrictive seccomp filters enabled by default.
- Cover SDK-managed and raw clone/UFFD process launch paths.
- Retain `disable_seccomp` only as an explicit development escape hatch.
- Make `sandbox doctor` fail its security check when the escape hatch is on.
- Run the lifecycle, snapshot, hibernation, UFFD, and burst suites on real KVM
  with the filters enabled.

Acceptance:

- No normal process command contains `--no-seccomp`.
- All live fleet paths pass with default Firecracker filters.
- A regression test covers each command builder.

Fleet evidence (2026-07-25, release `p0-secure-20260725-1`):

- Live Firecracker command lines contained no `--no-seccomp`.
- Held hot-created VMMs reported `NoNewPrivs: 1` and `Seccomp: 2` in
  `/proc/<pid>/status`.
- The full integration run exercised hot create, snapshot restore, batch
  snapshot clones, and hibernation wake with seccomp enabled.
- UFFD restore remains disabled in the production profile and therefore still
  needs a dedicated opt-in KVM validation before that mode can be enabled.

### P0.2 Jailer, privileges, and cgroups

Status: **next implementation slice**.

Use Firecracker's jailer (or a stricter equivalent) for every VM process:

- Dedicated unprivileged UID/GID allocation, preferably unique per running VM.
- Mount and PID namespace isolation, a per-VM chroot, and cgroup v2 placement.
- CPU, memory, process, file-descriptor, and block-I/O limits.
- Trusted, root-owned jailer inputs and chroot parent directories.
- Correct path translation for kernel, rootfs, snapshot state/memory,
  Firecracker API socket, log sink, and UFFD socket.
- Cleanup/reconciliation of abandoned jail directories and UID allocations.

The pinned SDK's naive jailer strategy does not cover this repo's raw clone and
UFFD paths. Implement one shared launcher abstraction first; do not jail only
cold boots.

Acceptance:

- Every Firecracker PID runs as its assigned non-root UID in the expected
  namespaces and cgroup.
- Snapshot, clone, wake, UFFD, and reconciliation tests pass through the same
  launcher.
- A guest workload cannot access another VM's jail files.

### P0.3 Bounded VMM output

Status: **implemented and fleet-verified**.

- Cap each `firecracker-<vm-id>.log`.
- Use mode `0600`.
- Continue draining output after the cap so diagnostics can never stall a VM.
- Close parent-side log descriptors on VM exit.
- Delete expected lifecycle-exit logs immediately.
- Retain unexpected-exit diagnostics for at most
  `firecracker_log_retention_hours`, capped to
  `firecracker_log_max_files`; never prune active VMM logs.
- Tighten retained legacy logs to mode `0600` during each sweep.
- Drain SDK-managed Firecracker API log FIFOs into the same bounded sink used
  for stdout/stderr.
- The guest serial device remains enabled for the current console workflow;
  its host-side output shares the bounded process sink.

Acceptance:

- Guest-influenced output cannot grow host files beyond the configured cap.
- A full-output guest cannot block the VMM.
- Disk-full behavior is covered by a failure test.

Fleet evidence (2026-07-25):

- New VMM log files were created with mode `0600`.
- The deployed config and `sandbox doctor` reported a 16 MiB per-VM bound.
- Unit tests prove writes after the cap are consumed without growing the file.
- The follow-up implementation removes expected-exit logs, bounds retained
  failure logs to 24 hours and 128 files by default, protects active logs, and
  consumes the SDK Firecracker log FIFO.
- Release `p0-secure-20260725-2` swept both production workers from hundreds of
  legacy mode-`0644` files to exactly 128 retained files, all mode `0600`.
- The focused lifecycle/snapshot/hibernation run passed 17/17, exercising both
  raw hot-clone and SDK-managed restore/wait/FIFO paths.

### P0.4 Guest network isolation

Status: **implemented and fleet-verified**.

- Default the bridge's guest-to-guest forwarding policy to `DROP`.
- Remove legacy `ACCEPT` rules during upgrade before installing the new policy.
- Keep outbound internet and host-to-guest agent/port-forward traffic working.
- Provide `allow_inter_guest_network: true` only for trusted deployments that
  intentionally use direct cross-sandbox service discovery.
- Longer term, use per-tenant network namespaces/policy rather than a shared
  bridge exception.

Acceptance:

- Two default sandboxes on one host cannot connect directly.
- Both retain outbound access and host-proxied API/port access.
- Restarting `serve` converges duplicate or legacy iptables rules.

Fleet evidence (2026-07-25):

- Two held sandboxes were placed on the same bridge and worker.
- A direct guest-to-guest connection to the peer's agent timed out.
- Host-proxied exec/files and explicit port forwarding continued to pass.
- The worker had the bridge-to-same-bridge `DROP` rule installed and
  `net.bridge.bridge-nf-call-iptables=1`.

### P0.5 Guest identity and SSH

Status: **focused gate implemented and fleet-verified; broader fault/resource automation pending**.

- Create a normal non-root interactive user.
- Run exec, files, PTY, and SSH as that user by default.
- Keep narrowly scoped privileged agent helpers only where required for clock
  and network re-identification.
- Remove the baked serial-console root password.
- Generate unique SSH host keys after every independent sandbox creation,
  including snapshot-derived sandboxes.
- Store authorized keys for the normal user and prohibit remote root login.
- Define an explicit opt-in privileged execution API rather than making every
  command root.

Acceptance:

- Default SDK/SSH sessions are non-root.
- Two clones never present the same SSH host key.
- A user process cannot modify privileged agent state.

### P0.6 Encrypted management transport

Status: **not started**.

- Support TLS on host and gateway TCP listeners, or require and verify a
  private authenticated reverse proxy.
- Separate worker-control credentials from client API credentials.
- Stop accepting bearer tokens in WebSocket query strings once browser access
  is served through a same-origin authenticated backend; until then, scrub
  query strings from every access log and trace.
- Keep the root-only Unix socket for local administration.

Acceptance:

- A production profile cannot bind a plaintext management listener to a public
  address.
- Gateway-to-worker traffic is authenticated separately from client traffic.
- Key rotation can occur without stopping the fleet.

### P0.8 Security verification

Status: **not started**.

Build host-level tests that execute on Linux/KVM and assert:

- seccomp and jailer/cgroup state from `/proc`;
- guest-to-guest denial and allowed egress;
- jail filesystem ownership and cross-jail denial;
- CPU, memory, process, descriptor, block-I/O, and log bounds;
- unique guest/SSH identity after hot create, snapshot create-many, and wake;
- cleanup after VMM crash, server crash, and host reboot.

Publish the tested host kernel, Firecracker, jailer, guest kernel, and rootfs
versions as release metadata.

`tests/security-gate.sh` now provides a repeatable direct-worker Linux/KVM gate
for the implemented controls. It creates two same-host sandboxes, verifies
seccomp and `NoNewPrivs` from `/proc`, log mode/size and expected-exit cleanup,
stale FIFO absence, the bridge firewall/sysctl, and guest-to-guest denial. It
does not yet cover the future jailer/cgroup limits, identity uniqueness, crash
injection, server crash, or host reboot cases listed above.

The gate passed independently on both active GCP workers on release
`p0-secure-20260725-2`. It also proved expected lifecycle deletion removes each
probe's log after the API destroy completes.

### P0 fleet validation record — 2026-07-25

Release `p0-secure-20260725-1` was rolled through the production GCP Nomad
system job while the fleet was idle. Both original workers became
release-compatible before validation traffic began. The validation burst also
started fresh MIG workers, proving the generated secure config applies on new
capacity rather than only on upgraded hosts.

Results:

- Local: `go test ./...`, Linux cross-build, shell syntax checks, config
  assertions, TypeScript typecheck, and `git diff --check` passed.
- Fleet quick suite: **33 passed, 0 failed**.
- Fleet full suite: **63 passed, 1 harness assertion mismatch**. The runtime
  correctly returned the SDK's specific `ConflictError` for a second pause;
  the stale assertion expected the base `SandboxError` name.
- Corrected hibernation suite: **5 passed, 0 failed**.
- Deployed `sandbox doctor`: **13/13 passed** on an original worker and a
  freshly autoscaled worker (Ubuntu 24.04 host, Firecracker v1.15.0).
- Direct host probes verified live seccomp state, root-only new VMM logs, the
  bridge firewall rule, and bridge netfilter.
- All validation sandboxes were deleted. The autoscaler began returning the
  temporary scale-out capacity to the configured suspended standby pool.

This first record validated only P0.1, the initial P0.3 slice, and P0.4. At
that release, aggregate log retention and a repeatable P0.8 gate were still
open; the follow-up record below closes those two bounded items. It did not,
and the follow-up still does not, declare the whole service production-ready.

### P0 fleet validation record — 2026-07-25 follow-up

Release `p0-secure-20260725-2` completed the P0.3 logging slice:

- Both active workers reported the release as compatible before probe traffic.
- `sandbox doctor` passed **14/14** on both workers, including the new retention
  check.
- `tests/security-gate.sh` passed independently on both workers.
- Expected-exit probe logs were deleted; retained failure diagnostics converged
  to the configured 128-file ceiling and mode `0600` on both hosts.
- The SDK Firecracker log FIFO is wired to the same bounded sink and no stale
  FIFOs were present.
- Focused fleet lifecycle, snapshot/batch-clone, restore, and hibernation tests
  passed **17/17**.

P0.3 aggregate retention is no longer open. Remaining P0 work is P0.2 jailer
and cgroups, P0.5 guest/SSH identity, P0.6 encrypted management transport, the
broader crash/reboot/resource cases in P0.8, and opt-in UFFD verification.

## P1: freeze a versioned API contract

### Contract foundations

- Add an OpenAPI 3.1 document as the HTTP source of truth.
- Introduce `/v1`; keep current routes as a compatibility adapter.
- Return RFC 9457 `application/problem+json` errors with stable `code`,
  `detail`, `request_id`, and field violations.
- Return `X-Request-Id` on every response and include it in logs.
- Add `Idempotency-Key` semantics for create, snapshot, pause, port-forward,
  batch, and terminate operations.
- Add opaque cursor pagination to sandboxes, snapshots, templates, operations,
  and hosts before those collections grow further.
- Add resource metadata and useful status/source/time filters.
- Remove PID, socket, tap, rootfs path, snapshot artifact paths, and other host
  internals from public objects.
- Move register, heartbeat, drain, release, adopt, and release-control routes
  under a separate internal listener or `/internal/v1` contract.

### Sandbox creation

One resource method creates a sandbox, regardless of source:

```http
POST /v1/sandboxes
Idempotency-Key: <opaque key>
Content-Type: application/json

{
  "name": "test-shard-1",
  "source": {"type": "snapshot", "id": "snap_123"},
  "lifecycle": {
    "ttl_seconds": 600,
    "idle_timeout_seconds": 300
  },
  "resources": {"vcpu": 2, "memory_mib": 1024},
  "metadata": {"run_id": "ci_456", "shard": "1"}
}
```

Supported sources:

- `{"type":"default"}`
- `{"type":"template","id":"tmpl_..."}`
- `{"type":"snapshot","id":"snap_..."}`

A user snapshot must be immutable and usable for independent creates while its
source sandbox continues running. Network identity relocation is an internal
concern and must not leak into the contract.

### Batch creation

Replace public fan-out with an explicit batch resource:

```http
POST /v1/sandbox-batches
Idempotency-Key: <opaque key>

{
  "count": 32,
  "sandbox": {
    "source": {"type": "snapshot", "id": "snap_123"},
    "lifecycle": {"ttl_seconds": 600}
  },
  "max_parallelism": 8
}
```

Return `202 Accepted` with an operation resource. Operation progress records
requested, succeeded, and failed counts. Final results contain one indexed
success or structured error for every requested sandbox. Never encode partial
success as a shorter bare array.

The server may continue using the optimized Firecracker clone implementation;
only the public abstraction changes.

### Lifecycle

- `POST /v1/sandboxes/{id}:pause`
- `POST /v1/sandboxes/{id}:resume`
- `DELETE /v1/sandboxes/{id}`
- `PATCH /v1/sandboxes/{id}` for name, metadata, TTL, and idle timeout

Pause/resume preserves the sandbox ID. Delete is permanent. Snapshot creation
creates an immutable reusable resource. The old 1:1 restore route becomes a
deprecated compatibility operation, not a v1 concept.

### Templates and snapshots

- Keep declarative/reproducible base environments as templates.
- Keep captured runtime state as snapshots.
- Hide the server's golden snapshot and expose its immutable revision as
  template metadata only when useful.
- Add snapshot retention, expiry, dependency-aware deletion, and durability
  state.

## P2: TypeScript SDK v1

Introduce a configured client:

```ts
const client = new SandboxClient({ baseUrl, apiKey })

const sandbox = await client.sandboxes.create({
  source: { snapshotId: "snap_123" },
  ttlMs: 600_000,
  idleTimeoutMs: 300_000,
})

const batch = await client.sandboxes.createMany({
  count: 32,
  source: { snapshotId: "snap_123" },
  maxParallelism: 8,
})

await sandbox.pause()
await sandbox.resume()
await sandbox.terminate()
```

SDK work:

- Resource collections: `sandboxes`, `snapshots`, `templates`, `operations`,
  and `portForwards`.
- `Operation<T>` polling/wait helpers and abort signals.
- Typed batch results with per-item errors.
- Cursor pagination exposed as `AsyncIterable`.
- Safe automatic retries with generated idempotency keys.
- Distinct `ttlMs`, `idleTimeoutMs`, `requestTimeoutMs`, and command
  `timeoutMs`.
- OpenAPI-generated transport types beneath a handwritten ergonomic layer.
- Normal npm publication with semver, provenance, changelog, and supported
  server-version metadata.

Compatibility:

- Keep static `Sandbox.create()` as a facade.
- Add source/snapshot creation and `Sandbox.createMany()`.
- Deprecate `Sandbox.restore()` and `Sandbox.fanout()`.
- Keep `hibernate()` and `kill()` temporarily as aliases for `pause()` and
  `terminate()`.
- Maintain old HTTP routes for at least one documented migration window.

## P3: operational readiness

- Request and lifecycle-stage latency histograms; distributed tracing across
  gateway, worker, and guest agent.
- SLOs for availability, create latency, wake latency, operation completion,
  snapshot durability, and cleanup.
- Snapshot retention, garbage collection, integrity verification, and restore
  drills.
- Ordered transactional SQLite migrations, backup/restore, and corruption
  handling.
- Gateway restart, worker loss, GCS outage, disk-full, OOM, corrupt snapshot,
  stale route, split-brain, and partial-network failure tests.
- Safe drain, rolling upgrade, compatibility matrix, and rollback procedures.
- CI for Go, TypeScript, race detection, linting, OpenAPI compatibility,
  dependency scanning, SBOMs, and reproducible signed artifacts.

## Recommended delivery order

1. Finish P0.1/P0.3/P0.4 verification on Linux/KVM.
2. Build the shared jailer launcher and complete P0.2.
3. Complete guest identity/SSH and management transport.
4. Add the P0 host-level security suite and make it a release gate.
5. Land the OpenAPI `/v1` foundations without changing legacy clients.
6. Implement create-from-source and batch operations.
7. Ship the SDK v1 facade and migration aliases.
8. Complete operational failure testing and SLO release gates.

The service should not be described as safe for arbitrary multi-tenant code
until all P0 acceptance criteria pass on the production host image.
