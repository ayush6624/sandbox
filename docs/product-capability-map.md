# Product capability map

Assessed September 7, 2026, with workspace evidence updated September 13.
This includes uncommitted work. The September 13 update ran the public SDK
workflow on the development fleet; runtime fixes discovered during verification
await rollout approval. Other capabilities retain their earlier evidence dates.
Older evidence does not establish every path on the latest release.

The fleet is used for development, with no current production serving, SLA, or
SLO. The proposed initial audience is developers running coding agents and
parallel experiments. That audience is a planning assumption, not validated
customer demand.

## Status definitions

| Status | Meaning |
| --- | --- |
| Usable | An implemented user workflow has recorded successful execution within the stated limits. This is not a production-readiness claim. |
| Partial | Components exist, but the complete workflow, reliability, distribution, or verification has a known gap. |
| Missing | No supported implementation was found in the inspected product interfaces and source. Examples or internal helpers do not establish a supported feature. |
| Deferred | Work is deliberately outside the current priority. Existing implementation and unresolved limitations are stated separately. |

## Start and use a workspace

| Capability and user outcome | Status | What exists and evidence | Remaining boundary |
| --- | --- | --- | --- |
| Create a sandbox and run commands | Usable | Default creates, exec, streaming output, files, and interactive shell. The September 5 dev canary passed create, exec, PTY output, terminal exit, and deletion. [Canary](development-canary-2026-09-05.md), [server routes](../internal/server/server.go), [SDK](../sdk/typescript/README.md). | The 25 sequential lifecycle samples describe a warm fleet, not sustained arrivals or cold-start tails. File APIs exist but were not separately reverified in this assessment. |
| Prepare dependencies once and reuse the environment | Usable | A public SDK example prepares a pinned repository, passes all 297 tests, saves a durable snapshot, and verifies independent reuse. Two- and eight-attempt runs and a separate reuse process passed. [Example](../sdk/typescript/examples/workspace/README.md), [verification](prepared-workspace-2026-09-13.md). | Users manage snapshot IDs and preparation scripts. Physical retained-byte accounting and garbage collection remain open. Runtime error fixes await deployment. |
| Build and manage a custom template without host access | Partial | Image-to-template CLI, template discovery, template sources, and per-template warm targets exist. [Build handler](../internal/server/template.go), [public template handlers](../internal/apiv1/handler.go), [warm templates](warm-templates-plan.md). | Building requires an operator on a host. A public remote build job with build logs and revision management is not implemented by the inspected routes. |
| Connect to apps and tools inside the sandbox | Usable | SSH, TCP forwarding, and a separate HTTPS edge have historical end-to-end evidence, including revoke and wake-on-connect. [Ingress](public-ingress.md), [edge](../services/sandbox-edge/README.md). | July verification is historical. Current endpoint and certificate health were not checked. SSH depends on the guest image containing sshd. |

## Save work and try parallel approaches

| Capability and user outcome | Status | What exists and evidence | Remaining boundary |
| --- | --- | --- | --- |
| Pause and continue the same workspace | Usable | Same-sandbox pause and resume preserve identity. All 25 lifecycle samples passed in the September 5 File-backend campaign. [Canary](development-canary-2026-09-05.md). | This evidence does not cover every cross-machine or lazy-memory path. |
| Create independent attempts from one saved state | Usable | Snapshots preserve process, memory, and disk state. Batches expose an indexed result for each requested child. All 18 workload clones passed the September 5 campaign. [Canary](development-canary-2026-09-05.md), [batch API](api-v1.md). | The [September 13 workspace test](prepared-workspace-2026-09-13.md) verifies that a child edit leaves siblings, source, and the saved snapshot unchanged. Snapshot resources remain fixed at capture. Arbitrary-concurrency and coordinator-restart workflow guarantees remain unverified. |
| Continue on another machine with lazy memory transfer | Partial | Lazy adoption, direct peer transfer, cloud fallback, and async handoff exist. State-integrity checks and several dev runs passed. [Lazy adoption](lazy-cross-worker-adoption.md), [peer transfer](peer-hibernation-transfer.md), [async handoff](async-peer-handoff.md). | Intermittent guest readiness timeouts remain unresolved. Debugging is deferred. Successful moves do not establish reliable continuation. |
| Recover accepted creation requests after a service restart | Usable | Single and batch create identities and results persist. Recorded worker assignment prevents an uncertain response from causing a second allocation. [Create recovery](create-operation-recovery.md). | Requires the stored database. Other mutations still use process-local replay. An unknown worker outcome can remain unfinished rather than move elsewhere. |
| Finish a snapshot backup after a worker process restart | Usable | Persisted upload jobs retry and expose progress or failure. Dev deployment and recovery verification are recorded. [Upload recovery](snapshot-upload-recovery.md). | Requires the original database and artifacts. A local snapshot is not durable before cloud publication completes. |
| Keep saved work with predictable storage growth | Partial | The [bounded worker cache](chunk-cache.md) and [generation handoff ownership](chunk-storage-design.md) pass local checks. Reader and publisher recovery cover process exit with the registry intact. [Cloud receipts](verification/chunk-cloud-recovery-2026-09-13/README.md) recover completed owned handoff backups after local source loss. New-format writes default off. | Deployment, provider verification, lost-volume recovery, legacy migration, incomplete backups without source, and automatic collection remain open. Long-term storage bounds and fleet reclamation are not established. |

## Integrate with agent workflows

| Capability and user outcome | Status | What exists and evidence | Remaining boundary |
| --- | --- | --- | --- |
| Use the service from TypeScript | Usable | Resource client, lifecycle methods, batches, operation waits, errors, and examples. The dev canary exercised SDK behavior. [SDK](../sdk/typescript/README.md), [canary](development-canary-2026-09-05.md). | Distribution is documented through release tarballs. Current source changes are not proof of a newly published SDK release. |
| Use a supported general-purpose Python SDK | Partial | An async Python client supports evaluation adapters. [Client](../examples/rl-environments/sandbox_client.py). | This is an example client, not an independently packaged SDK with a declared support and API-parity contract. |
| Run existing agent evaluation suites | Partial | Harbor and verifiers adapters exist. The evaluation plan records a Harbor smoke, negative control, and 10-task sweep with no infrastructure exceptions. [Plan and results](rl-environments-plan.md), [adapters](../examples/rl-environments/README.md). | Full-suite and current-release coverage are incomplete. The verifiers integration has an upstream registration patch. Adapter documentation conflicts with the later verification record. |
| Use a managed visual desktop session | Missing | No supported desktop-session product interface was found in the inspected API and SDK documentation. | Shell and app forwarding do not provide a managed desktop session. This has no assigned implementation priority. |

## Understand and control the service

| Capability and user outcome | Status | What exists and evidence | Remaining boundary |
| --- | --- | --- | --- |
| See operation results and understand slow progress | Partial | Request IDs, durable create operations, indexed errors, upload state, logs, and metrics exist. [API](api-v1.md), [create recovery](create-operation-recovery.md). | September 13 fixes persist original create request IDs and classify absent sources as 404; they pass local restart/replay tests and await rollout. Users still cannot follow a complete persisted sequence from placement through restore and guest readiness. A supported lifecycle event feed was not found. |
| Track resource use | Usable | Usage intervals, allocated-resource totals, CPU consumption, and per-sandbox utilization APIs exist, with historical verification described in their plans. [API](api-v1.md), [usage plan](usage-metering-plan.md), [metrics plan](sandbox-metrics-plan.md). | The usage API covers reporting live hosts. Complete billing history relies on the durability store. This is not a complete invoicing or cost-per-successful-task product. |
| Absorb simultaneous starts economically | Partial | Placement, queueing, autoscaling, default and template warm pools, and burst benchmark tools exist. [Warm templates](warm-templates-plan.md), [burst plan](burst-absorption-plan.md). | Current sustained-arrival limits and the best warm-capacity settings remain unmeasured. |
| Run isolated guests under operator control | Usable | Jailer, seccomp, cgroups, credential-domain separation, and historical security gates exist. [Readiness plan](production-readiness-plan.md), [management security](management-security.md). | Historical release gates cover specified launch profiles. They do not establish current security coverage for every new lazy restore path or tenant authorization. |
| Offer separate customer accounts with quotas | Deferred | Operator and worker credentials are separated, but per-tenant authorization, quotas, and rate controls are explicitly excluded from the existing workstream. [Readiness plan](production-readiness-plan.md). | Shared-service account controls are not implemented by the inspected public contract. No production launch is planned here. |
| Set destination-specific network access per sandbox | Missing | Host networking and inter-guest controls exist. No per-sandbox destination-policy contract was found in the public API. [Contract](../api/openapi.yaml), [provisioner](../internal/provisioner/provisioner.go). | Requires a user-facing policy model and enforcement. No current priority is assigned. |
| Remove guest address changes from startup | Deferred | Namespace provisioning and jailer support exist, but creation paths do not use them. [Identity plan](guest-identity-cost-plan.md). | Explicitly backlogged. Any speedup needs fresh measurements; this is not a proven fix for the guest stall. |

## Evidence conflicts and limits

- `api-v1.md` describes template sources as default-only, and `templates.md`
  says discovery returns only the built-in image. Current `normalizeSource`,
  `listTemplates`, and `getTemplate` accept and expose custom templates.
  The map follows source behavior, without claiming that the entire template
  workflow has fresh fleet verification.
- The evaluation README says no live tests have run. The later evaluation plan
  records completed phases 0 through 3. This map reports the narrower recorded
  runs, not full-suite readiness.
- The ingress document contains old proposed-gap text alongside its implemented
  description. It is evidence of historical work, not a current endpoint check.
- Old production plans and historical timing headlines are not current release
  certifications. This map retains dates and mode limits rather than treating
  a passing test on one path as proof for all paths.

The [September 5 ASCII review](ascii-competitive-review-2026-09-05.md) is the
historical motivation for this work. This assessment maps our product; it does
not refresh competitor claims or establish a provider ranking.

Proposed feature priorities and acceptance criteria are in the
[product roadmap](product-roadmap.md). Supporting engineering work and deferred
issues remain in the [development backlog](development-backlog.md).
