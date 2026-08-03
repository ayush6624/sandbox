# Audit log plan

Status: **design**. Nothing below is built yet.

Goal: a durable, queryable record of who did what to which sandbox, when, and
with what result. Motivated by security forensics, invoice dispute resolution,
and the compliance questions that arrive with enterprise customers.

## This is not a billing input

Filed separately from [usage-metering-plan.md](usage-metering-plan.md) on
purpose. An audit log is the wrong source for usage, structurally — not merely
inconvenient. Most billable interval boundaries in this system have **no API
call behind them**:

- the idle-hibernation reaper freezes on a timer (`internal/server/hibernate.go`)
- the TTL reaper destroys on `expires_at` (`internal/server/ttl.go:90`)
- `cleanupExitedMachine` closes out a VM that died on its own
  (`internal/server/machine_watch.go:32`)
- `shutdownAll` hibernates everything on serve shutdown — what a MIG scale-in
  triggers
- a raw TCP connection to a forwarded port **wakes a hibernated sandbox**
  (`internal/server/portproxy.go`), opening a billable interval with no request
  to audit at all

Deriving usage from request records would mean replaying a state machine from
the one evidence source with systematic blind spots. The usage ledger lives at
the transitions instead, which is the only place that can see them.

The inverse, though, is a requirement: those same authorless events are exactly
what customers escalate about ("who deleted my sandbox?"), so the audit log
must record **system actors** — `system:ttl-reaper`, `system:idle-hibernate`,
`system:shutdown`, `system:reconcile` — with the same fidelity as human ones.
That is the whole reason this document is not just "turn on access logging".

Join key between the two: `sandbox_id` plus `request_id`. A disputed invoice
line goes ledger interval → the call that created the sandbox.

## What exists today

- `httpapi.Middleware` already emits one structured line per request —
  `request_id`, method, path, status, `duration_ms` — and echoes an
  `X-Request-Id` header (`internal/httpapi/httpapi.go:36`, log at `:48`). It
  goes to stdout, so it lands in journald/Nomad task logs: not durable past log
  rotation, not queryable, no actor, no resource extraction, no outcome detail.
- `/v1/operations` is an in-memory map that is actively pruned
  (`internal/apiv1/handler.go:31`, `pruneOperationsLocked` at `:719`). Useful
  for polling a batch; not a trail.
- Nothing records system-actor events at all.

So the work is three specific gaps — an actor, a durable sink, and capture at
the tenant boundary — not a greenfield build.

## The identity blocker

There is currently no caller identity to log. `Credentials.MatchAuthorization`
is a set-membership test returning `bool`
(`internal/management/credentials.go:135`), and `bearerAuth` uses it to place a
request in one of three trust domains — client, worker, edge
(`internal/gateway/gateway.go:1763`). A match does not reveal *which* token
matched. An audit log written against today's primitives records "some holder of
a client token, from IP X", which is adequate for forensics and close to
useless for a billing dispute.

**Prerequisite, worth doing before the rest:** have credential matching return a
stable, non-secret **key label** (`key_id`) alongside the boolean — the index or
a configured name per token, never the token itself. It is a small change to
`internal/management`, it turns every audit entry from anonymous to
per-credential, and it is the same primitive per-tenant auth will need later
(P0.7 in [production-readiness-plan.md](production-readiness-plan.md)), so it is
not throwaway work.

Until that lands, `actor.key_id` is `"unknown"` and the log is a forensics tool
only. That limitation should be stated in the doc that ships with it rather than
discovered during an audit.

## Capture point: the gateway

Audit at the **gateway**, not per-worker. It is the tenant boundary, and it is
the only place that sees the whole truth:

- it replaces the tenant credential with the owning worker's before proxying, so
  a worker-side log records the internal identity, not the caller's
- it alone sees requests that never reach a worker: placement failures, queue
  waits and timeouts, the 404-vs-503 resolution, failover between hosts
- it is where WebSocket upgrades (`/shell`, `/connect/{port}`) authenticate

Workers additionally emit audit entries for the **system-actor** events listed
above, since those originate on the worker and the gateway never hears about
them. Two writers, one schema, one bucket prefix per host id.

### Stateless-compatible

The gateway deliberately holds no durable state — it rebuilds routing from
heartbeats and self-heals on restart. Auditing does not violate that, provided
it is strictly **write-only**: buffer in memory, flush to object storage, and
never read it back to make a decision. The invariant is about *decision* state,
not exhaust. A bounded buffer that drops on overflow (with a counter, see
metrics below) is correct here — audit must never be able to stall or fail a
tenant request.

## Event schema

One JSON object per event, newline-delimited:

```json
{
  "id": "gw-01:1754179200123:00042",
  "ts": "2026-08-03T12:00:00.123Z",
  "actor":    {"kind": "client", "key_id": "acme-prod", "ip": "203.0.113.7"},
  "action":   "sandbox.create",
  "resource": {"type": "sandbox", "id": "b1f2..."},
  "request":  {"id": "req-...", "client_id": "...", "method": "POST",
               "path": "/v1/sandboxes", "idempotency_key": "..."},
  "outcome":  {"status": 201, "duration_ms": 12, "error_code": ""},
  "detail":   {"vcpus": 2, "mem_mib": 1024, "source_type": "default"}
}
```

- `actor.kind` is `client` | `worker` | `edge` | `system`. For `system`,
  `key_id` names the subsystem (`ttl-reaper`, `idle-hibernate`, `shutdown`,
  `reconcile`) and `ip` is absent.
- `action` is a closed vocabulary (`sandbox.create`, `sandbox.destroy`,
  `sandbox.hibernate`, `sandbox.wake`, `sandbox.exec`, `sandbox.shell.open`,
  `sandbox.files.read`, `sandbox.files.write`, `sandbox.ssh_access.grant`,
  `port.expose`, `snapshot.create`, …) rather than a derived method+path, so
  queries survive route changes.
- `request.id` is server-assigned and authoritative. `X-Request-Id` is
  **client-suppliable** (`internal/httpapi/httpapi.go:22`), so it is recorded
  separately as `request.client_id` and never used as a primary key.
- `detail` is an allowlist per action, never the raw body. See below.

## What must not be recorded

This log will contain customer activity, so data minimization is a design
constraint, not a policy afterthought:

- **Never the `Authorization` header.** Also never the WebSocket subprotocol
  value: `/shell` and `/connect/{port}` carry the credential as
  `sandbox.bearer.<base64url(token)>` (`internal/wsutil`), so a naive "log the
  negotiated subprotocols" line would write bearer tokens to durable storage in
  a form that trivially decodes. Redact to the scheme prefix.
- **Never query strings verbatim.** Query credentials are rejected at the door
  and both proxies strip them, but a rejected request is precisely one worth
  auditing, and it is the one most likely to carry a secret in the URL.
- **`exec` commands and file contents: hash by default.** Command lines
  routinely carry credentials (`export TOKEN=`, `curl -H "Authorization: …"`).
  Default to `{"cmd_sha256": "…", "cmd_len": N}` plus argv[0], with full capture
  as an explicit, per-deployment opt-in. Forensic value is mostly in *which*
  sandbox ran *how much* exec, and the hash still correlates repeats.
- **SSH public keys** are not secret; record a fingerprint rather than the key
  for size, and never anything from `/ssh-key` beyond that.

## Durability

Same spool pattern as usage, separate bucket:

```
audit/<component>/<host_id>/<YYYY-MM-DD>/<flush_seq>.jsonl
```

- Immutable, append-only objects, one flush per object, each writer owning its
  own prefix so there is no CAS contention. Uses the existing
  `internal/gcsblob` client, mirroring the commit-marker discipline in
  `internal/server/hib_durable.go`.
- A **separate bucket** from usage and snapshots, because the readers and
  retention differ and the IAM should: grant writers
  `roles/storage.objectCreator` **only** — not `objectAdmin`. A log that the
  logging host can rewrite or delete is not evidence. Pair with bucket-level
  retention/object-hold so history is immutable even to an admin mistake.
- Flush every ~30 s or 1000 events, whichever first, plus on graceful shutdown.
  At-least-once; consumers dedup on `id`, which is why `id` is deterministic
  (`<component>:<ms>:<seq>`) rather than random.
- Retention: 1 year is the usual floor for the compliance conversation this
  serves; set it by bucket lifecycle rather than in code.

## Metrics

Added to the existing expositions (host `/metrics`, gateway `/metrics`):

```
sandbox_audit_events_total{action}          # low cardinality: closed vocabulary
sandbox_audit_dropped_total                 # buffer overflow — alert on any nonzero
sandbox_audit_unflushed_events              # spool falling behind
sandbox_audit_flush_failures_total
```

No `sandbox_id` or `key_id` labels, for the same cardinality reason the usage
plan keeps per-sandbox detail out of Prometheus.

## Phases

**Phase A — `key_id` prerequisite.** `internal/management` returns a
non-secret credential label; `bearerAuth` attaches it to the request context.
Independently useful, and unblocks meaningful actor fields.

**Phase B — event plumbing.** Internal `audit.Recorder` (bounded buffer, closed
action vocabulary, redaction helpers); gateway middleware emitting
client/edge/worker events; worker emission for the four system actors.
Stdout sink only, structured — already an improvement on today's line.

**Phase C — durable spool.** GCS writer, flush loop, `objectCreator`-only IAM,
bucket lifecycle + retention policy, metrics and the drop alert.

**Phase D — read path.** Deliberately last, and deliberately thin: a query API
implies an index, an index implies state, and the gateway must not hold it. Most
likely shape is an offline query over the bucket (BigQuery external table over
the JSONL, or `gsutil cat` plus `jq` for incident work) rather than an endpoint.
Revisit only if support workflows prove it necessary.

## Tests

- Redaction is the part that fails silently and expensively, so it gets direct
  unit tests: an `Authorization` header, a `sandbox.bearer.<b64>` subprotocol,
  and a credential-bearing query string must each be absent from the encoded
  event. A table test over the closed action vocabulary asserts every action has
  an allowlist and that unlisted `detail` keys are dropped.
- `request.client_id` never populates `request.id` (client-supplied values
  cannot become primary keys).
- Bounded buffer: overflow drops and increments `sandbox_audit_dropped_total`
  rather than blocking the request path — assert a full buffer does not delay a
  handler.
- System actors: a TTL-reaped and an idle-hibernated sandbox each produce an
  event with `actor.kind = "system"` and the right `key_id`.
- Spool: replay after a crash between write and stamp dedups on `id`.

## Open questions

- **Does the edge belong in this log?** `services/sandbox-edge` terminates
  public ingress and sees request volume no gateway entry captures. Its access
  log is arguably a different artifact (traffic, not control-plane actions), and
  merging them would swamp the control-plane signal.
- **Failed-auth events.** High value for forensics, and an easy amplification
  vector: an unauthenticated flood becomes an unbounded write bill. Needs
  aggregation (count per source per window) rather than one event per rejection.
- **Whether `sandbox.exec` is logged per call at all.** Under the pty/exec
  stress profile this is the highest-volume action by far, and a per-call event
  may be better replaced by a per-session summary on close.
