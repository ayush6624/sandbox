# Create operation recovery

Public single creates and batches retain accepted work across gateway restarts.
The coordinator stores the request before dispatch, and the worker links each
request member to its allocation in SQLite. Losing a response does not authorize
a second allocation.

The base durable-create mechanism was deployed to the development gateway and
both workers on September 6, 2026 as
`fb09662-dev-20260906070836`.

This guarantee covers `POST /v1/sandboxes`, `POST /v1/sandbox-batches`, and their
operation records. Other mutation routes retain their existing process-local
idempotency behavior. Snapshot upload recovery is a separate mechanism.

## Replay behavior

The idempotency scope combines the HTTP method, path, and `Idempotency-Key`.
The coordinator hashes the raw request body. Reusing a key with different bytes,
including changed JSON whitespace, returns `409`. The current shared-credential
service does not add a separate tenant identity to this scope.

A batch returns `202` only after its specification, member identities, and
original response are committed together. Replaying the request returns the
original `202` body and operation ID, including after the batch finishes.
`GET /v1/operations/{id}` supplies current progress and indexed results.

A single create commits acceptance before execution and waits for its terminal
result. Its eventual `201` response or terminal problem is stored for replay.
Disconnecting the client stops that HTTP wait. The service dispatcher still owns
the accepted operation.

Successful results are historical. If a sandbox is later deleted, replay returns
its original create result and ID. It does not create a replacement. A current
`GET /v1/sandboxes/{id}` can therefore return `404` for an ID in a successful
historical operation.

## Single-create handles awaiting deployment

`POST /v1/sandbox-creations` accepts the same body as `POST /v1/sandboxes` and
returns `202` with a real `sandbox_create` operation and one member. `Location`
identifies its operation URL. GET and list now include both single and batch
operations, including historical synchronous single creates.

Every successful async POST returns the exact original acceptance receipt,
including after success, failure, or coordinator reopen. GET supplies current
progress and results. The synchronous `/v1/sandboxes` endpoint still waits for
its stored final `201` or problem response.

Idempotency is scoped to the endpoint. The same key sent to the synchronous and
async endpoints represents two different operations. Recover through the same
endpoint with the same key and body. SDK and CLI async creates never fall back
to another create endpoint or follow a mutation redirect. Older servers reject
the new endpoint without returning an operation handle.

`client.sandboxes.createAsync()` and `Sandbox.createAsync()` return the existing
SDK Operation handle. `operation.wait()` returns terminal indexed results,
including failures. It does not throw merely because a member failed. Its total
timeout covers polling, retries, and response-body reads. Canceling a wait stops
the client, while accepted work continues. `CreateAcceptanceError` retains the
idempotency key when a response is lost or cannot be validated. Retry identical
options with that key to recover the receipt.

The CLI uses durable creation by default:

```sh
sandbox up --idempotency-key preview-42
sandbox up --async --idempotency-key preview-42
sandbox operation get OPERATION_ID
sandbox operation wait OPERATION_ID
```

The first two commands are alternatives. With identical create options and the
same key, either recovers the same receipt. Normal `up` writes changing stages
and recovery details to stderr, waits for coordinator completion, and prints the
actual legacy sandbox JSON to stdout. `--progress` explicitly selects this same
behavior. `--async` prints the receipt JSON and exits.
`operation wait` prints terminal operation JSON and returns a nonzero exit code
if any member failed. Progress includes the last completed worker stage, and
terminal errors include public member failure details.

Both durable modes print their key before submitting and their operation ID
after acceptance. If interrupted before the ID arrives, repeat the original
create options with that key. Once the ID is known, use `operation get` or
`operation wait`. Neither polling nor interruption deletes the sandbox or
submits another create. If fetching the legacy sandbox after successful creation
fails, the CLI reports the known sandbox and operation IDs.

Normal `up` accepts independent `--vcpus` and `--mem` overrides and
`--hibernate-after=-1` to disable idle hibernation. Missing or zero resources use
the selected worker's defaults. Negative resources and nonzero memory below
128 MiB are rejected before submission.

Use `sandbox up --legacy` explicitly with older servers. It cannot be combined
with `--async`, `--progress`, or `--idempotency-key`. A failed durable create never
automatically switches to that path. The new endpoint, option support, and
default progress behavior have not been deployed.

## Resource defaults and idle hibernation

Create requests accept independent `resources.vcpu` and `resources.memory_mib`
overrides. Missing or zero values use the chosen worker's template defaults. A
positive override forces a cold create for the default source. An empty or
all-zero object means no override and preserves warm eligibility. Snapshot and
custom-template sources retain their captured resources and reject positive
overrides.

The worker resolves defaults before allocation and memory admission. The
registry and VM options therefore use the same effective resources. Sandbox
responses still return complete positive resource values, even when the create
request omitted one or both values.

`lifecycle.idle_timeout_seconds` accepts `-1` to disable automatic idle
hibernation, `0` to use the host default, or a positive timeout. Values below
`-1` are invalid. This applies to creation and lifecycle updates. Updating TTL
on a sandbox with disabled idle hibernation preserves `-1`. TTL itself remains
nonnegative. In the SDK, `idleTimeoutMs: -1` uses the same sentinel; other values
remain milliseconds.

## Assignment survives an unknown response

Before dispatch, the coordinator commits the selected host ID and worker
`registry_id`. This database identity survives a worker process restart with the
same SQLite file. A replacement database gets a different identity, even if its
host ID or address is reused.

An unanswered command stays assigned to that worker database. The coordinator
retries the recorded assignment when the worker becomes reachable. It does not
place the member elsewhere after a timeout, stale heartbeat, or changed database
identity. These members remain unfinished while their outcome is unknown.

A known capacity failure is terminal in this implementation. Placement can wait
for the configured queue deadline before returning a terminal failure. The
coordinator does not move a rejected member to another worker. Replaying the old
key returns that failure. A new key represents an intentional new create attempt.

## Allocation and readiness share durable records

The worker accepts explicit member identities through the authenticated
`POST /internal/v1/create-commands` endpoint. Each identity belongs to one
normalized create specification and can acquire at most one sandbox ID.

Cold allocation inserts the starting sandbox and request link in one transaction.
Warm promotion keeps the existing warm VM's ID and commits the request link,
public fields, and successful result together. Source and metadata are present
at allocation instead of being patched after creation.

`Registry.MarkRunning` publishes readiness and the successful request result in
one transaction. `Registry.Destroy` keeps successful outcomes and commits an
unfinished allocation's failure with deletion of its sandbox row. Request
records do not cascade with sandbox deletion. The September 6 deployed build
uses `create_interrupted` for these failures; the local change below preserves
specific startup causes.

## Startup failure causes awaiting deployment

Local code records the first safe cause before startup teardown. The request
remains `allocated` until teardown commits, so recording a cause cannot make an
unknown live VM look like a completed failure or permit another allocation.
If deletion rolls back, the allocation and pending cause remain intact.
Registry reopen and subsequent reconciliation retain that cause. Completed
successes and failures cannot be overwritten by a later cleanup error.

| Code | Failed startup step |
| --- | --- |
| `rootfs_prepare_failed` | Filesystem preparation |
| `network_setup_failed` | Host networking setup |
| `vm_start_failed` | VM creation, start, or process identification |
| `guest_readiness_failed` | Guest agent readiness |
| `guest_identity_failed` | Guest identity initialization |
| `create_publish_failed` | Recording the started VM or publishing readiness |

These postallocation failures return HTTP 500 with fixed public messages.
Internal paths and raw guest responses remain in operator logs. A canceled
execution context does not establish a guest failure; teardown keeps the
`create_interrupted` fallback when no earlier specific cause was recorded.
Preallocation missing-source and validation errors retain their existing codes.

The regression exercises a real filesystem-preparation failure through worker
execution, teardown, and replay. Registry tests verify first-cause retention,
reopen, atomic deletion rollback, and unchanged successful historical outcomes.
This fixes terminal error attribution locally. The worker now delivers durable
stage observations to its coordinator. Public stage projection is implemented locally as described below.
Single-create handles and default CLI progress are implemented locally as described above.

Worker startup still destroys non-hibernated VMs and their stale registry rows.
This change preserves outcomes, not running VMs across worker restart. A completed
request retains historical success after cleanup. An allocated request that never
reached readiness keeps its specific failure, or the interruption fallback, and
cannot allocate again.
Cancellation before allocation leaves no terminal worker failure, so the same
assignment can retry.

Snapshot work uses chunks of at most eight explicit members and respects the
operation's `max_parallelism`. Each member retains its own identity and outcome;
results do not depend on the completion order of a compact success list.

<a id="worker-stage-journal-awaiting-delivery-and-deployment"></a>

## Worker stage journal and coordinator delivery

The local implementation records worker admission, source preparation, sandbox
preparation, VM start, guest network setup for clones, agent readiness, guest
identity setup, and publication. Each member retains its current stage and last
completed stage, with an attempt number and increasing observation sequence.
Progress reads use a separate database connection and remain available while
execution waits for admission or a snapshot lock.

A warm claim records ready directly. It does not claim that VM startup ran for
that request. Cancellation before allocation leaves an observation that a retry
can replace with a new attempt. An older attempt cannot advance or allocate.

Allocation and terminal progress commit in the same transactions as the worker
request ledger. A failed progress write stops forward execution. After clone
launch returns a machine, cleanup retains that machine and its allocation until
stopping succeeds. Cold-start PID and publication failures use the same bounded
cleanup. A cold launch with an unconfirmed exit also retains its allocation.

The journal retains the latest unacknowledged observation. A background sender
delivers it to the accepting coordinator while execution continues. Each request
keeps its coordinator identity and delivery target. Local requests use the local
store, and gateway requests use the configured gateway and worker credentials.
Legacy assignments remain anonymous after a worker upgrade.

The coordinator checks the recorded worker assignment, database identity, request
hash, and observation sequence before committing an update. It acknowledges only
the sequence received. A lost acknowledgement causes a retry, and an older
acknowledgement cannot discard a newer worker observation. The sender scans
pending rows with a rotating cursor so a rejected owner does not block later rows.

A successful stage observation does not complete the operation or publish a route.
The existing Execute retry obtains current routability before recording success.
Terminal failures can complete their member in the ingestion transaction.

Race tests cover blocked admission during background delivery, lost
acknowledgements, coordinator reopen, stale updates, ownership and credential
rejection, legacy upgrades, and terminal success before the Execute reply.
Public API and SDK stage projection are implemented locally. This code has not
been deployed or verified against real VM startup gates. Sustained delivery load
and a total history storage limit remain open. CLI progress is the default for new builds.

The native clone and UFFD restore adapters now return the retained machine
handle when failed-launch termination cannot be confirmed. Waiting on that
handle checks the Firecracker child or its dedicated cgroup, independently of
the launcher process. Cleanup remains pending until that check succeeds.
Wake and adoption retries use current machine and registry state to decide
whether a handoff can reopen; an earlier launch error cannot override a later
confirmed rollback.

The [Linux process regression log](verification/create-progress-2026-09-13/native-launch-linux.log)
records seven passing test groups, including 24 failed-launch combinations and
surviving-child checks. These ran on local Linux arm64 with real child processes
and fake Firecracker API responses. They do not prove KVM guest startup or the
deferred migration readiness issue.

## Public progress awaiting deployment

`GET /v1/operations/{id}` and `GET /v1/operations` include `progress` for each
indexed result, including members that have no terminal result yet. `request_id`
identifies the request that originally accepted the operation. Replaying a batch
POST still returns the exact original acceptance response. Read GET for fresh
progress.

Operation listing reads only the requested page, ordered by creation time and
operation ID. Continuation tokens keep their position when newer operations are
accepted. Numeric tokens from older builds remain accepted; responses return
the current opaque token format. Each page includes fresh status and progress,
so values may change between requests. This paging change is locally verified
and awaits deployment.

Each progress object separates coordinator dispatch from the last delivered
worker observation:

| Field | Meaning |
| --- | --- |
| `coordination.phase` | `queued`, `placing`, `assigned`, `retrying`, or `completed` |
| `coordination.updated_at` | When the phase began. Repeated retries retain this time. Older records omit it. |
| `worker.current` | The last observed worker stage and its attempt and start time |
| `worker.last_completed` | The last stage known to have completed, including its attempt and completion time |
| `worker.condition` | `active`, `tearing_down`, `succeeded`, or `failed` |
| `worker.observed_at` | When the worker recorded this observation |
| `worker.sequence` | Increasing sequence used to order this member's worker observations |

`queued` includes waiting for the operation's `max_parallelism` limit. `placing`
means placement has started, and can include its capacity wait. `assigned` means
the coordinator committed a worker assignment. It does not establish worker
admission or readiness. `retrying` means an Execute response left the outcome
unknown. Retrying does not erase a delivered worker stage or its last completion.

The `worker` object is absent until an observation arrives. Legacy workers never
supply it. Observations can lag execution. A worker can report `succeeded` while
the coordinator is still confirming the result. Existing operation status,
`completed_at`, and indexed sandbox or error results determine completion.

The TypeScript SDK maps this to `item.progress.coordination` and
`item.progress.worker`. All timestamps become `Date` values, including nested
stage marks. `operation.state.requestId` retains the original request ID.
Progress is optional for older servers, and the SDK preserves unknown stage
strings. Stages are observations, not a percentage or a universal ordered list.

## Historical success does not restore a route

The internal worker outcome includes a separate `Routable` observation of its
current registry. That observation is distinct from its stored successful result.
The gateway uses current routability and database identity when installing routes.
Replaying a deleted sandbox's historical success does not publish a new route or
overwrite a route that already belongs to another host.

## Database ownership and paths

The installed gateway uses
`--operation-db /var/lib/sandbox-gateway/operations.db` under systemd's
`StateDirectory=sandbox-gateway`. The standalone gateway flag defaults to
`operations.db`; deployments must choose a persistent path explicitly.

A worker keeps request outcomes and its database identity in its existing
registry database. Direct-worker public creates use a coordinator database at
`<registry-path>.operations.db`. Both gateway and worker start recovery with the
service lifecycle and join their dispatchers before closing operation storage.

History is retained indefinitely in this implementation. Disk usage grows with
accepted operations and worker requests. Terminal worker progress stores stage,
owner, attempt, sequence, and timestamp metadata. Reads reconstruct its outcome
from the same historical request ledger used for replay, including after the
sandbox is deleted. New terminal writes do not duplicate the full sandbox result
in the progress snapshot.

The worker converts older inline outcomes in pages of at most 32 records per
delivery-loop iteration, including iterations with no pending delivery. It
removes an inline outcome only after comparing it with the ledger. A mismatching
record remains intact and does not prevent later records from being converted.
Pending delivery and historical reads return the same complete observations
before and after conversion. Acknowledgement counters and stage timestamps do
not change. Freed SQLite pages can be reused; this does not promise a smaller
database file on disk.

Recovery uses a partial index containing pending/running operations and completed
single creates still awaiting their final response. Its one-second poll no longer
scans finalized operation history. Index creation scans existing rows once during
database migration. These storage changes are locally verified and await deployment.
Once converted, progress requires a binary that supports compact outcome reads;
an older progress implementation cannot decode it correctly.

A total storage cap and history expiration policy remain future work. Local
SQLite protects process restart with disk intact; it does not
provide recovery after that disk or database is lost. Credentials remain in live
configuration and are not part of persisted operation specifications.

The implementation is split by ownership:

- [createops](../internal/createops/) stores acceptance, assignments, and outcomes.
- [APIV1 dispatch](../internal/apiv1/create_operations.go) owns public replay and recovery.
- [Gateway execution](../internal/gateway/create_operations.go) owns placement and worker transport.
- [Worker execution](../internal/server/create_requests.go) validates and joins member commands.
- [Worker request records](../internal/registry/create_requests.go) link allocation and historical results.

## Verification

The integrated [race checks](verification/create-recovery-2026-09-06/race-tests.log)
passed for the coordinator store, public API, gateway, registry, worker, and CLI:

```sh
go test -race ./internal/createops ./internal/apiv1 ./internal/gateway \
  ./internal/registry ./internal/server ./cmd/sandbox
```

Tests exercise transaction rollback during acceptance, allocation, warm promotion
and readiness publication; SQLite reopen; exact replay; cancelled client waits;
fresh and recovered chunk concurrency; indexed partial results; replacement
registry fencing; and retained outcomes after destruction.

The [live report](verification/create-recovery-2026-09-06/live-recovery.json)
records a successful drill using the deployed binary and both real workers.

| Observation | Result |
| --- | --- |
| Lost-response boundary | Proxy held a successful worker reply before gateway receipt |
| Abrupt gateway restart | PID 140828 replaced by 140838 with the same operations database |
| Batch replay after restart | Byte-identical original 202 body and Location; replay header present |
| Ordinary batch | Three original distinct sandbox IDs, no extra allocations |
| Snapshot batch | Nine distinct clones with all indexed results and guest execution |
| Changed body with original key | 409 for both batches |
| Single create, second gateway restart | Original 201 bytes replayed after PID changed to 140850 |
| Replay after deletion | Original public and worker results retained, no sandbox recreated |
| Cleanup | No errors or remaining sandboxes from the run |

The temporary snapshot source and its copies also passed
[cleanup verification](verification/create-recovery-2026-09-06/live-source.json).
The main gateway's full REST and WebSocket PTY smoke test passed after rollout.
Its separate [v1 smoke](verification/create-recovery-2026-09-06/main-smoke.json)
also confirmed durable create, exact replay, guest execution, and cleanup using
the installed gateway database.

Final [source](verification/create-recovery-2026-09-06/final-source.json) and
[target](verification/create-recovery-2026-09-06/final-target.json) probes found
eight warm guests and 9440 MiB reserved on each worker, with no empty cgroups,
unfinished worker requests, pending public operations, or upload jobs. The
[inventory](verification/create-recovery-2026-09-06/final-inventory.json) preserves
the pre-existing hibernated sandbox and snapshot. Fourteen completed worker
request records remain as intentional replay history.

These are correctness checks; they do not establish latency percentiles or an SLA.

The live verifier runs from the control VM after deployment. It starts an isolated
gateway against real workers, holds a successful worker response, kills the
gateway, and restarts it with the same database. It checks exact replay, result
indices, worker inventory, guest execution, and replay after sandbox deletion.

```sh
cd ~/sandbox-src
. infra/gcp/fleet-secrets.env
export HOST_TOKEN GATEWAY_CONTROL_TOKEN
python3 scripts/verify-create-recovery.py \
  --source-url http://10.128.0.28:8080 \
  --target-url http://10.128.0.35:8080 \
  --output .audit/operations/create-recovery-live.json
```

An optional `--snapshot-id <durable-snapshot-id>` adds nine snapshot clones.
The verifier preserves the supplied snapshot and removes only its own sandboxes.
Its report includes response evidence, process IDs, and cleanup results.
