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

The enabled host-isolation paths and public API contract now have substantial
GCP evidence. Destructive resource-boundary behavior and live credential
rotation have dedicated passing gates, and the rebuilt worker image has passed
the combined contract, SDK/e2e, security, and recovery sequence. The remaining
release risks are operational readiness, reproducible artifact production, and
the explicitly deferred shared-service tenant controls.

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
one-to-many input. Pause/resume is the one-to-one lifecycle operation.
"Fan-out" can remain in internal implementation and compatibility-route
discussions, but public API, SDK, and benchmark labels should use snapshot
source and batch-create terminology.

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

Overall status: **complete for the enabled production profile**. The rebuilt
image passed the two-worker isolation and recovery gates, the combined
contract/SDK/e2e regression gate, resource-exhaustion/isolated-ENOSPC, and live
credential rotation. UFFD restore remains disabled and is not covered by this
claim; it requires its own KVM security gate before enablement. P0.7
per-tenant authorization, quotas, and API rate/concurrency limits remain
excluded from this workstream by product decision, so this is not a claim that
the service is ready for arbitrary mutually untrusted tenants.

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

Status: **implemented and fleet-verified for enabled production paths**.

Detailed design: [P0.2 shared jailer and cgroup design](p0-jailer-design.md).

Use Firecracker's jailer (or a stricter equivalent) for every VM process:

- Dedicated unprivileged UID/GID allocation, preferably unique per running VM.
- Mount and PID namespace isolation, a per-VM chroot, and cgroup v2 placement.
- CPU, memory, process, file-descriptor, and block-I/O limits.
- Trusted, root-owned jailer inputs and chroot parent directories.
- Correct path translation for kernel, rootfs, snapshot state/memory,
  Firecracker API socket, log sink, and UFFD socket.
- Cleanup/reconciliation of abandoned jail directories and UID allocations.

The shared launcher routes cold boot, snapshot restore, hot clone, and UFFD
restore through one `ProcessLauncher`, with explicit mode, host-visible API
socket, and idempotent post-exit cleanup contracts. The GCP production profile
enables the jailer, assigns per-VM identities, creates a private mount/PID
namespace and chroot, and places each VMM in a preconfigured cgroup v2 leaf
with CPU, memory, PIDs, and block-I/O limits. Snapshot peak memory has an
explicit bounded allowance. Cleanup handles expected deletion, VMM crashes,
server crashes, and host reboot reconciliation.

Acceptance:

- Every Firecracker PID runs as its assigned non-root UID in the expected
  namespaces and cgroup.
- Snapshot, clone, wake, UFFD, and reconciliation tests pass through the same
  launcher.
- A guest workload cannot access another VM's jail files.

Fleet evidence (2026-07-27):

- `tests/security-gate.sh` passed independently on both active workers first
  on `a97b68f` and again on rebuilt worker release `a223889`.
- Live VMMs ran under distinct non-root UID/GID assignments, chroots,
  namespaces, and cgroup leaves with the configured resource controls.
- Snapshot batch creates produced distinct identities and isolation state.
- Expected deletion and injected VMM crash cleanup passed.
- The server-crash and host-reboot recovery gates passed, including stale
  runtime reconciliation after service recovery.
- UFFD remains disabled in the production profile; it is not covered by this
  release claim.

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

Status: **implemented and fleet-verified on the rebuilt image**.

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

Evidence:

- The rootfs initializes a normal `sandbox` user, removes the baked root
  password, rejects SSH root login, and gives privileged identity/network
  operations only to narrowly scoped helpers.
- Independent sandbox and snapshot-derived creates rotate guest identity and
  SSH host keys; the two-worker security gate verified unique identity and
  non-root SSH behavior on `a97b68f` and rebuilt worker release `a223889`.
- The subsequent live `/v1` contract rerun exposed a transient
  `ssh.service` startup failure after restore. Commit `a223889` adds bounded
  startup retry and unit coverage. Worker image
  `sandbox-worker-20260727-000120`, matching golden data image
  `sandbox-golden-data-20260727-001237`, and template
  `sandbox-workers-tpl-20260727-002100` were rolled through the MIG. The
  rebuilt fleet then passed the complete contract/e2e and security/recovery
  sequence.

### P0.6 Encrypted management transport

Status: **implemented and live-rotation verified**.

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

Evidence:

- Production listener validation rejects insecure public plaintext binds,
  preserves the root-only local Unix socket, and supports TLS or a verified
  private management boundary.
- Client, worker-control, and administrative credentials have separate
  domains. Cross-domain use is rejected by tests and gateway/worker routing
  preserves the worker credential.
- Bearer-token query strings are rejected and request logging scrubs sensitive
  query data.
- Local transport and credential-domain tests pass.
- `tests/security-token-rotation-gate.sh` passed against the live GCP gateway:
  both keys worked during overlap, only the replacement worked after
  retirement, and the original root-owned `0600` file was restored
  byte-for-byte without restarting the gateway.

### P0.8 Security verification

Status: **implemented and fleet-verified for enabled production paths**.

Build host-level tests that execute on Linux/KVM and assert:

- seccomp and jailer/cgroup state from `/proc`;
- guest-to-guest denial and allowed egress;
- jail filesystem ownership and cross-jail denial;
- CPU, memory, process, descriptor, block-I/O, and log bounds;
- unique guest/SSH identity after hot create, snapshot create-many, and wake;
- cleanup after VMM crash, server crash, and host reboot.

Publish the tested host kernel, Firecracker, jailer, guest kernel, and rootfs
versions as release metadata.

`tests/security-gate.sh` is the repeatable direct-worker Linux/KVM release
gate. It now covers seccomp and `NoNewPrivs`, per-VM jail identity and
namespaces, cgroup controls, bounded VMM output, guest and SSH identity,
guest-to-guest denial with allowed egress, snapshot batch isolation, VMM crash
cleanup, and expected lifecycle cleanup. `tests/security-recovery-gate.sh`
covers server-crash and host-reboot reconciliation.

Both active GCP workers passed the complete security gate on release
`a97b68f` and again on rebuilt worker release `a223889`. The server-crash and
host-reboot recovery gates also passed.
`tests/security-exhaustion-gate.sh` then passed on the disposable second
worker: guest address-space, process, and file-descriptor limits failed within
their bounded rlimits; an isolated 32 MiB loop filesystem reached deterministic
ENOSPC without consuming production storage; the real VMM cgroup/log, control
sandbox, API, and host free-space boundaries remained healthy; and exact
cleanup succeeded.

Immutable rebuilt-image metadata:

- worker image `sandbox-worker-20260727-000120`, image ID
  `4020148980465518867`; golden data image
  `sandbox-golden-data-20260727-001237`, image ID
  `4459982300482619317`; template
  `sandbox-workers-tpl-20260727-002100` (`n2-standard-16`);
- Ubuntu 24.04 host kernel `6.17.0-1021-gcp`, Firecracker and jailer v1.15.0;
- guest kernel `6.18.36+`, kernel SHA-256
  `25576852390f4883c913ba26c42e6de5569c8a0cff6769800924db8631a3b6c3`;
- Ubuntu 24.04 rootfs, ext4 UUID
  `ac1c575a-3944-44ad-8953-030155a71aef`, size 10,737,418,240 bytes,
  SHA-256
  `13d7b4a1cccd6f60c2f9b3d9877bee35f2903a4ea7d9fa84747945802cea46c7`;
- golden snapshot `93372ba7-f355-4df7-b58b-935f93da3061`, full format,
  isolation signature `jailer-v1`, with manifest SHA-256
  `bf6e04142b602943197086b48afe41674d7acb0fe1f471f297f54450d8cf591b`.

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

P0.3 aggregate retention is no longer open. This paragraph records the state
of the 2026-07-25 release; the later P0 implementation and validation record is
below.

### P0 fleet validation record — 2026-07-27

Current repository and gateway release: `ea0f707`. Rebuilt worker release:
`a223889`.

Confirmed:

- Both active GCP workers passed the full security gate on rebuilt release
  `a223889`, including jailer UID/GID, namespaces/chroot, cgroups and I/O
  bounds, seccomp, bounded output, non-root guest/SSH identity, unique
  snapshot-derived identities, network isolation and egress, and
  lifecycle/VMM-crash cleanup.
- Server-crash recovery passed after adding the worker supervisor and delegated
  cgroup handling.
- Host-reboot recovery passed, including service return and stale-runtime
  reconciliation.
- The rebuilt fleet passed the live `/v1` contract and SDK probe. The complete
  API/SDK e2e suite passed **64/64 in 360.7 seconds**, with its self-cleaning
  report at
  `tests/results/run_2026-07-27T07-44-47-400Z.json`.
- Local validation currently reports **207 Go tests**, the relevant Go race
  suite, **49 TypeScript SDK/benchmark tests**, TypeScript typecheck and build,
  deterministic OpenAPI generation, Linux builds, and syntax validation of
  **29 shell scripts**. These counts must be reconfirmed at the final release
  commit.
- The worker image, golden data image, template, host and guest kernels,
  Firecracker/jailer, rootfs, and golden manifest identifiers are recorded
  under P0.8 above.
- The running gateway embeds revision
  `ea0f7077b8d325eef52619b89622967f4b0e5fe8` and has SHA-256
  `73f7a6215247342668eeef040862266533bc63c14af3cf5cbb87d7a84eb3d55b`.
  The running worker embeds revision
  `a223889ef53b88874bc5db548bc6c184884758aa` and has SHA-256
  `30d97aaa8771300282ca6c75a299e116cce1d643992a8b842e4cd8544c7a6a8d`.
  Its immutable GCS `sandbox` and `sandboxd` object generations are
  `1785099673553318` and `1785099661793884`, respectively. Both binaries
  report Go 1.25.3.

P0 scope boundaries:

1. UFFD requires its own KVM security gate before it can be enabled; it remains
   disabled and outside the current production profile.
2. P0.7 shared-service tenant authorization, quotas, and API
   rate/concurrency limits remain explicitly deferred.
3. The deployed gateway and worker binaries embed the expected full VCS
   revisions but report `vcs.modified=true`. Their exact SHA-256 checksums and
   immutable GCS generations identify what was tested, but a clean,
   reproducible artifact build remains a P3 release-engineering gap.

## P1: freeze a versioned API contract

Status: **implemented and GCP fleet-verified on the hardened image**.

The OpenAPI 3.1 contract lives at `api/openapi.yaml`. Both worker and gateway
serve `/v1` through a compatibility adapter, with sanitized resources,
request IDs, RFC 9457 problems, idempotent mutations, cursor pagination,
filters, source-based creation, explicit pause/resume/delete/PATCH lifecycle,
port-forward resources, snapshot retention/durability state, dependency-aware
snapshot deletion, templates, and indexed asynchronous batch operations.
Fleet coordination aliases now also live below `/internal/v1`.

Idempotency records and operation resources are process-local for at most 24
hours. Persisting them across gateway restarts is deliberately coupled to the
transactional storage and migration work in P3; clients should still retry
with the same key because a live process provides exactly-once replay.

Fleet evidence (2026-07-26, release `p1-api-20260726-2`):

- Both Nomad worker allocations ran the release with zero release mismatches
  and 96 allocatable slots after rollout.
- The self-cleaning `/v1` contract probe passed creation, RFC 9457 errors,
  request IDs, idempotent replay, filtering/pagination, PATCH, port forwards,
  pause/resume, reusable snapshots, indexed batch operations, dependency
  conflicts, deletion, and cleanup.
- The legacy lifecycle/snapshot/clone/hibernation suites remained compatible:
  **17 passed, 0 failed**.
- Both workers passed the direct security gate for seccomp, bounded VMM logs,
  FIFO cleanup, guest isolation, and lifecycle cleanup.
- GCP exposed an empty-body in-process proxy panic during the first probe. The
  regression was fixed in `dc2d2f3`, covered by normal and race tests, and the
  complete gate passed on the corrected immutable release.

This evidence remains valid for the versioned contract implementation, but it
is now supplemented by the hardened-image `/v1`, SDK probe, and 64/64
API/SDK e2e pass on worker release `a223889`.

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

Status: **implemented and GCP fleet-verified on the hardened image; npm
registry publication pending**.

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

SDK evidence (2026-07-26, `sandbox@1.0.0`):

- OpenAPI-generated transport declarations sit below a handwritten configured
  client with `sandboxes`, `snapshots`, `templates`, `operations`, and
  `portForwards` collections.
- The SDK gate passed **35/35** tests, strict typecheck, build, deterministic
  OpenAPI regeneration, and `npm pack --dry-run`.
- The publishable package has zero runtime dependencies and `npm audit
  --omit=dev` reported zero vulnerabilities. Provenance, changelog, semver, and
  supported `/v1` metadata are configured; publishing is intentionally not
  performed without an explicitly selected npm registry/package authority.
- The self-cleaning GCP SDK probe passed configured creation, commands, port
  forwards, pause/resume, snapshot-source `createMany`, operation polling,
  async pagination, stable problem errors, and full cleanup against
  `p1-api-20260726-2`.
- The probe and full API/SDK e2e suite passed again on hardened worker release
  `a223889`; the current local SDK/benchmark suite reports 49 passing tests,
  subject to final release-commit confirmation.
- `npm audit --omit=dev` remains clean with zero runtime vulnerabilities. The
  full development audit reports four high-severity findings in the
  OpenAPI-generator toolchain; they do not ship at runtime, but must be
  remediated or explicitly accepted before publication.

## P3: operational readiness

Status: **open**.

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

Completed operational slices include server-crash and host-reboot recovery,
bounded resource exhaustion and isolated ENOSPC, live credential rotation,
single-writer autoscaling correctness, hardened benchmark cleanup, and
multi-shape autoscaling correctness. P3 remains open because:

- autoscaling create correctness is fixed, but create p95 under the rigorous
  held-load campaign remains outside the release SLO and needs performance
  work;
- the deployed gateway and worker artifacts report `vcs.modified=true`, so
  clean reproducible builds, signing, and provenance are not yet demonstrated;
- the development dependency audit has four high-severity generator-chain
  findings, despite a zero-vulnerability runtime dependency audit;
- the tracing/SLO, transactional storage, broad outage matrix, backup/restore,
  CI, SBOM, and signed-artifact items above remain incomplete.

## Recommended delivery order

1. Preserve the passing `a223889` rebuilt-image `/v1`, SDK/e2e, security, and
   recovery evidence and reconfirm final local test counts.
2. Produce clean reproducible gateway and worker artifacts from the release
   commit, publish their signed provenance/SBOMs, and rerun the release smoke
   gate against those exact checksums.
3. Bring autoscaling create p95 inside its release SLO while retaining the
   passing correctness, capacity, and deterministic-cleanup properties.
4. Resolve or formally accept the development-only generator-chain audit
   findings, select npm publication authority, and publish the SDK.
5. Complete the remaining P3 failure testing, observability/SLO gates,
   transactional storage, backup/restore, and rollback work.

The enabled production profile has passed its P0 runtime and host-security
gates, but the service must not be described as safe for arbitrary mutually
untrusted tenants until the deferred P0.7 tenant authorization, quota, and
rate/concurrency controls are implemented and verified.
