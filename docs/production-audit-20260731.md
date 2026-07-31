# Production readiness audit — 2026-07-31

Scope: a line-by-line read of the Go implementation at `8425a0c` (main), plus the
infra scripts that wire credentials and capacity. ~36 k lines across
`cmd/`, `internal/`, `services/sandbox-edge/`, `infra/gcp/`.

**Method and its limits.** This is primarily a static read; no benchmark was run
and nothing was injected on the fleet. Findings are labelled:

- **[reproduced]** — I wrote a probe test and watched it fail. Two findings
  (C1, C2 — both High) are in this category; the probes are in Appendix A and
  become regression tests when the fixes land.
- **[verified]** — the defect follows directly from the cited code paths.
- **[inferred]** — depends on runtime conditions I could not observe (kernel
  behaviour, real filesystem, actual VMM overhead). Two findings only.

Every claim carries `file:line` against `8425a0c`; 16 of the load-bearing ones
were re-checked against the tree after writing. Where
`docs/production-readiness-plan.md` already tracks an item I say so rather than
re-litigating it.

Severity is about production impact, not code quality:

| | meaning |
| --- | --- |
| **Critical** | breaks a trust boundary or loses user data with no operator action |
| **High** | user-visible outage, data loss, or unrecoverable resource leak under normal load |
| **Medium** | degrades correctness/latency/cost under realistic load, or a confusing contract |
| **Low** | latent, cosmetic, or needs an unusual trigger |

---

## 1. Executive summary

The runtime is unusually well built for its age: the lifecycle state machine is
explicit, capacity is admission-checked, crash reconciliation is real, and the
hard parts (clone re-identification, diff snapshots, UFFD, subprotocol auth on
WebSockets) are done carefully and documented in-line. Most of what follows is
not "this is sloppy" — it is "this is the next layer of failure, and it will
show up in production".

Ten things I would fix before treating this as a service other people depend on:

1. **The gateway hands the worker control token to anyone holding a client API
   key** (`GET /route/{id}`, `GET /raw-route/{port}`). The edge's credential
   *is* `SANDBOX_API_KEY`. → S1, Critical.
2. **A client API key can zero fleet placement capacity** (`PUT /worker-release`)
   **or drain a host** (`POST /hosts/{host}/drain`). → S2, High.
3. **Deleting a hibernated sandbox can fail permanently, after destroying its
   durable record** — a partial-index collision on a reused tap/IP. The TTL
   reaper then retries forever. → C1, High.
4. **A failed clone-path wake corrupts the sandbox's identity mapping**, making
   it permanently unwakeable. → C2, High.
5. **A transient GCS error during shutdown destroys running user sandboxes**
   instead of freezing them. → C3, High.
6. **`POST /snapshots/{id}/fanout` has no `count` bound** and bypasses the create
   semaphore — one API call can flatten a worker. → C4, High.
7. **The jailer's per-VM memory allowance (mem+512 MiB) disagrees with admission
   accounting (mem+156 MiB)** — the parent cgroup can OOM and take every VM on
   the host with it. → X1, High.
8. **One SQLite connection serves the control plane and the data plane**, with an
   O(N)-queries-per-heartbeat pattern on top. This is the host's scaling
   ceiling. → X2, High.
9. **The in-guest agent is unauthenticated**; sandbox↔sandbox isolation rests on
   a single, never-re-verified iptables rule. → S3, High.
10. **An unknown sandbox id costs up to 5 minutes**, and that path is reachable
    from the unauthenticated public edge. → L1, High.

Counts: 1 Critical, 8 High, 24 Medium, 12 Low.

---

## 2. Security and trust boundaries

### S1 — Client credential obtains the worker control credential — **Critical** [verified]

`internal/gateway/gateway.go:1629`

```go
workerOnly := r.URL.Path == "/register" || strings.HasPrefix(r.URL.Path, "/internal/v1/")
```

`GET /route/{id}` and `GET /raw-route/{port}` are not in that set, so they
authenticate with the **client** credential. Both return the worker's bearer
token verbatim:

- `internal/gateway/route.go:48-52` — `edgeRoute{HostAddr, Token: snap.token}`
- `internal/gateway/raw.go:520-524` — `"host_addr": …, "token": h.token`

And the edge's credential is the same token users get:

- `infra/gcp/edge.sh:79` — `printf '%s' "${GATEWAY_TOKEN:?}" | … secrets versions add "$TOKEN_SECRET"`
- `infra/gcp/control.sh:162` — `SANDBOX_API_KEY=${GATEWAY_TOKEN}`
- `infra/gcp/rollout.sh:409` — `SANDBOX_API_KEY=<gateway-token>`

So any API-key holder can call `/route/<any id>`, receive the worker control
token, and then talk to that worker's TCP listener directly. That grants:

- every sandbox on the host (`/exec`, `/files`, `/shell`, `/connect/{port}`),
  bypassing the gateway's routing and accounting entirely;
- the internal control routes — `bearerAuth` on the worker admits
  `/internal/v1/*`, `/adopt`, `/release` on the worker credential
  (`internal/server/server.go:511-523`).

This is the more striking because the codebase enforces the two-domain split
everywhere else: `Credentials.Overlaps` checks at
`internal/server/server.go:431-435` and `internal/gateway/gateway.go:277-280`,
and `serve` outright refuses a fleet worker with no separate worker token
(`server.go:428-430`). `/route` and `/raw-route` undo all of it.

Caveat, stated plainly: `docs/production-readiness-plan.md` explicitly defers
per-tenant authorization, so today one shared client key is already
all-powerful *at the gateway*. The finding is still Critical because (a) it
escalates past the gateway to the worker control plane, (b) it silently defeats
the credential separation the rest of the code pays for, and (c) it makes the
eventual multi-tenant story a rewrite rather than an addition.

**Fix.** Give the edge its own third credential domain, and stop returning the
worker bearer to it. The durable shape is a short-lived capability scoped to one
sandbox id (and optionally one guest port), minted by the gateway and verified
by the worker's `CONNECT` handler — the edge never needs host-wide authority.
Interim: a distinct `--edge-token`, `/route` and `/raw-route` moved under
`workerOnly`, and worker tokens rotated.

### S2 — Client credential performs fleet control operations — **High** [verified]

Same root cause, different blast radius. On non-`workerOnly` paths:

- `PUT /worker-release` (`gateway.go:347`, handler at `:1482`) sets
  `expectedRelease` and forces `h.slotsFree = 0` on every host whose release
  doesn't match (`:1527-1531`). One request with a bogus release string is a
  **fleet-wide create outage** that survives gateway restarts (it's persisted
  to `releaseFile`, `:1514-1523`).
- `POST /hosts/{host}/drain` (`gateway.go:367`, handler at `adopt.go:119`)
  freezes and migrates every sandbox on a host.
- `GET /hosts` (`gateway.go:344`) discloses fleet topology, per-host addresses,
  and capacity.

The `/internal/v1/` twins of these routes *are* correctly gated
(`gateway.go:348-349, 368`). The legacy paths are the hole; they exist only for
compatibility.

**Fix.** Move `/worker-release`, `/hosts`, and `/hosts/{host}/drain` under the
worker/operator domain and delete the ungated aliases.

### S3 — Unauthenticated in-guest agent behind one unmonitored firewall rule — **High** [verified]

`cmd/sandboxd/main.go:37-43` registers `/health`, `/exec`, `/exec/stream`,
`/files`, `/dir`, `/shell` with **no authentication**. Only four privileged
endpoints are gated, and that gate is a source-IP check
(`cmd/sandboxd/controlauth.go:21-40`). The agent listens on `:8090` on all
interfaces (`main.go:33, 61`).

What stops sandbox A from running commands in sandbox B is one appended rule:

```go
// internal/provisioner/provisioner.go:133-139
return []string{"FORWARD", "-i", bridge, "-o", bridge, "-j", target} // DROP by default
```

installed once per `serve` startup (`:90-97`). Properties that make this
uncomfortable:

- it depends on `br_netfilter` + `bridge-nf-call-iptables=1` (`:55, 63, 119-131`);
- it is **appended** to `FORWARD`, so anything that inserts an earlier `ACCEPT`
  (Docker, firewalld, an operator, a `iptables-restore`) silently defeats it;
- nothing re-asserts or verifies it after startup, and no metric or alert
  reports its state;
- the config default is correct (`AllowInterGuestNetwork: false`,
  `internal/config/config.go:68-73`) but a single `true` is a fleet-wide
  cross-tenant RCE.

Mitigating: `configureGuestCommandProcess` drops exec/shell to uid 1000
(`cmd/sandboxd/guestuser_linux.go:16-36`) and file ops go through `setfsuid`
(`:42-90`), so this is tenant-level compromise, not guest root.

**Fix.** Defence in depth, in order of value: (1) authenticate sandboxd with a
per-sandbox secret delivered via MMDS at `/identity` time — the plumbing already
exists; (2) re-assert the isolation rule on a timer and export
`sandbox_guest_isolation_ok`; (3) consider a per-VM netns or `ebtables`/nftables
rules bound to the tap rather than the bridge.

### S4 — `vm_isolation` defaults to `direct` — **Medium** [verified]

`internal/config/config.go:231-233`: an omitted `vm_isolation` becomes
`"direct"`, and `Load` accepts it (`:332-334`). A config that forgets the key
runs Firecracker unjailed, as root, with no per-VM uid, cgroup, chroot, or PID
namespace — and nothing in `serve` warns. The doc comment says "jailer is
required for production" (`:148-150`), which is exactly the kind of requirement
that belongs in code.

**Fix.** Default to `jailer`; require an explicit `"vm_isolation": "direct"` and
log a loud warning when it's chosen.

### S5 — `route_localnet=1` retained for a retired feature — **Medium** [verified]

`internal/provisioner/provisioner.go:62` sets
`net.ipv4.conf.all.route_localnet=1` host-wide. `CLAUDE.md` documents it as
back-compat for the removed DNAT scheme and calls it harmless. It weakens a
kernel default that exists to stop 127/8 being routable, on a host that also
runs an untrusted-workload bridge. DNAT is gone; the sysctl should go with it.

### S6 — Forwarded host ports bind all interfaces, unauthenticated — **Medium** [verified]

`internal/server/portproxy.go:85` — `net.Listen("tcp", net.JoinHostPort(f.bind, …))`
with `bind == ""` outside tests. Guest ports mapped to host ports are exposed
on every interface with no authentication; only the cloud firewall separates
them from the internet. That is a legitimate product decision, but it should be
a deliberate, configurable one (`port_proxy_bind`), not the consequence of an
empty string.

### S7 — Source-IP authentication for privileged agent endpoints — **Low** [verified]

`cmd/sandboxd/controlauth.go:33-37` admits `/clock`, `/identity`,
`/snapshot-poll`, `/ssh-key` when the peer address equals the guest's default
gateway. Guest workloads run as uid 1000 and cannot forge a source address, and
the reachable damage is confined to the sandbox's own state, so this holds
today. It is still an IP-equality check on a shared L2 segment; a shared secret
would cost nothing.

---

## 3. Correctness and race conditions

### C1 — Hibernated-sandbox delete can fail permanently, after destroying the durable record — **High** [reproduced]

> Reproduced with the probe in Appendix A.1:
> `REPRODUCED C1: destroying hibernated A fails permanently: constraint failed:
> UNIQUE constraint failed: sandboxes.guest_ip (2067)`


Three facts compose into this:

1. The partial unique indexes now include `stopping`
   (`internal/registry/registry.go:378-379, 419-420`).
2. A hibernated row **keeps its stale `tap_device`/`guest_ip`** — `Hibernate`
   clears pid/vm_id/socket but not the identity (`registry.go:869-881`).
3. Once the pool is tight, the pickers hand a hibernated row's tap/IP to a new
   sandbox — that is the explicit second pass
   (`registry.go:1628-1657`: "then — only when the pool is otherwise exhausted
   — allowing them"). `Wake` does the same (`:915-923`).

Now delete that hibernated sandbox. `destroyLocked`
(`internal/server/server.go:1138-1189`) runs, in order:

```go
s.cancelHibernationUpload(id)
if err := s.invalidateHibernationRecord(ctx, id); err != nil { return err }  // deletes hib/<id>/record.json
if err := s.reg.MarkStopping(ctx, id); err != nil { … }                       // ← UNIQUE violation
```

`MarkStopping` (`registry.go:790-808`) moves the row into the index set while it
still carries a tap/IP a *running* sandbox now owns → `UNIQUE constraint failed`
→ 500 on `DELETE`, deterministically, forever.

Consequences, in order of severity:

- **The GCS commit marker is already gone** when the failure happens. The
  sandbox is now local-only; if the host dies it is unrecoverable, and no
  cross-host adopt can find it.
- The sandbox becomes **undeletable** via the API.
- The TTL reaper retries every 10 s (`internal/server/ttl.go:51-69` →
  `destroyExpired` → `destroyLocked`), each pass re-deleting the marker and
  logging a failure — so an expired hibernated sandbox **never** gets reaped and
  its rootfs, hibernation artifacts, and reserved host port leak permanently
  while the log fills.
- `shutdownAll`'s straggler pass (`server.go:619-633`) hits the same wall.

Trigger is ordinary: a busy host (occupancy high enough that the soft-avoid pass
fails) with hibernation enabled — i.e. exactly the density scenario hibernation
exists for.

**Fix.** Clear `tap_device`/`guest_ip` in `Hibernate` (they are meaningless for
a frozen VM; `Wake` reallocates anyway and the soft-avoid heuristic can key on a
separate `last_tap`/`last_ip` pair). Alternatively drop `stopping` from the two
partial indexes, or skip `MarkStopping` for rows already `hibernated`. Also
reorder `destroyLocked` so the durable record is invalidated only *after* the
row transition succeeds.

### C2 — Failed clone-path wake corrupts the identity mapping — **High** [reproduced]

> Reproduced with the probe in Appendix A.2:
> `REPRODUCED C2: after a failed wake the row says 172.16.0.11 but the frozen
> memory has 172.16.0.10; the next wake sees 172.16.0.11 free, takes the
> same-identity path, and boots a guest whose real IP is 172.16.0.10`


`registry.Wake` (`registry.go:888-936`) persists a **new** tap and guest IP when
the old pair is taken:

```go
if _, err := tx.ExecContext(ctx,
    `UPDATE sandboxes SET status=?, stopped_at=NULL, tap_device=?, guest_ip=? WHERE id=?`,
    StatusRunning, sb.TapDevice, sb.GuestIP, id); …
```

`rollbackWake` (`internal/server/hibernate.go:603-611`) undoes the VM and the
tap but **not the identity**:

```go
_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
if err := s.reg.Hibernate(context.Background(), sb.ID); …   // status only
```

So after a `wakeClone` failure (a `StartClone` error, a GARP-then-agent timeout
at `hibernate.go:936`), the row records the *new* identity while the frozen
memory still has the *old* one baked in. On the next wake attempt the new
tap/IP are free, so `same == true` (`registry.go:915`), and `wakeRestore` loads
the snapshot on its old baked IP while `waitForAgent(ctx, sb.GuestIP, …)` polls
the new one (`hibernate.go:675`). It times out; the wake fails; the row rolls
back; repeat. **The sandbox is permanently unwakeable**, and every subsequent
`exec`/port connection pays the full 30 s before failing.

**Fix.** Have `Wake` return the pre-wake identity (or record it), and restore it
in `rollbackWake`. A regression test is straightforward: hibernate, occupy the
tap, force `wakeClone` to fail, assert the row's tap/IP are unchanged.

### C3 — A GCS error during shutdown destroys running sandboxes — **High** [verified]

`resetHibernationDurability` (`internal/server/hib_durable.go:264-281`) returns
an error if **any** of six object deletes fails:

```go
for _, object := range []string{ hibRecordObj(id), hibManifestObj(id), … } {
    if err := s.blob.Delete(ctx, object); err != nil {
        return fmt.Errorf("delete stale %s: %w", object, err)
    }
}
```

`hibernateWithMode` propagates it (`hibernate.go:374-376`), and `shutdownAll`
interprets *any* hibernate failure as licence to delete:

```go
// internal/server/server.go:601-604
if err := s.hibernate(ctx, id, true); err != nil {
    fmt.Fprintf(os.Stderr, "[%s] shutdown hibernate failed (%v), destroying\n", id, err)
    _ = s.destroy(context.Background(), id)
}
```

So a Nomad task stop, an autoscaler scale-in, or a MIG standby cycle that
coincides with a GCS blip **deletes every running user sandbox on that host**,
where the entire design intent is that shutdown freezes and never destroys
(`server.go:570-578`). This fires whenever `snapshot_bucket` is configured,
which production does.

The same coupling breaks the delete path: `destroyLocked` →
`invalidateHibernationRecord` (`hib_durable.go:285-293`) returns an error on a
GCS failure, so **you cannot delete a sandbox — or reap an expired one — while
GCS is unavailable**.

**Fix.** Object-store availability must never gate VM lifecycle. Make durability
cleanup best-effort, record "this generation is not adoptable" locally (a marker
file next to the artifacts is enough), and reconcile GCS lazily. Keep the strict
behaviour only for the explicit cross-host `release` path, where a caller *is*
asking for a durability guarantee.

### C4 — Unbounded `count` on fanout — **High** [verified]

`internal/server/snapshot.go:656-659` validates only `count >= 1`. A single
authenticated call with `count: 100000`:

- allocates `clones := make([]*clone, body.Count)` (`:722`);
- phase 1 is bounded at 8 (`sem := make(chan struct{}, 8)`, `:724`) — but
  **phase 2 spawns one goroutine per clone with no semaphore at all**
  (`:749-768`), each running a 30 s agent wait;
- issues `count` registry transactions, each doing a full `loadUsed` table scan,
  on the single-connection SQLite (`registry.go:336, 1529`);
- **does not acquire `createSem`** — unlike `handleCreate` (`server.go:684`) and
  `handleRestore` (`snapshot.go:439`) — so it bypasses the very mechanism that
  exists to stop boot storms;
- holds `snapshotLock(snapID)` throughout (`:673-675`), blocking restores and
  deletes of that snapshot for the duration.

The gateway makes it worse: `handleSnapshotOp`
(`internal/gateway/snapshots.go:76-136`) forwards the whole request to **one**
host and applies the capacity accounting only after it returns
(`:167-181`), so placement is blind for the entire fanout.

`POST /v1/sandbox-batches` gets this right — `count` capped at 100,
`max_parallelism` 1..32 (`internal/apiv1/handler.go:486-499`). The legacy
endpoint it delegates to does not.

**Fix.** Cap `count` (match v1's 100), gate each clone on `createSem`, bound
phase 2 with the same semaphore as phase 1, and fail fast when
`count > FreeSlots`.

### C5 — Snapshot delete dependency check is not transactional — **Medium** [verified]

`registry.DeleteSnapshot` (`registry.go:1268-1285`) counts dependents and then
deletes as two separate statements, and `deleteSnapshotLocked`
(`snapshot.go:1018-1034`) does its own count earlier still. The single
connection serializes each statement but not the pair. A `restore`/`fanout` that
commits between the count and the delete leaves a live sandbox referencing a
snapshot whose row and artifacts are gone. The per-snapshot operation lock
(`snapshotLock`) covers restore/fanout/delete on the same host, so the window is
narrow — but `ensureSnapshotLocal`-driven pulls on *other* hosts aren't covered.

**Fix.** Do the dependency check and delete in one transaction.

### C6 — v1 create is not atomic and can leak sandboxes — **Medium** [verified]

`internal/apiv1/handler.go:198-218`:

```go
rec := h.call(r, http.MethodPost, path, legacyBody)              // sandbox now exists
…
annotated := h.call(r, http.MethodPatch, ".../public-fields", fields)
if annotated.Code >= 200 && annotated.Code < 300 { … } else {
    return Sandbox{}, annotated.Code, legacyDetail(annotated)     // ← sandbox left running
}
```

Any failure of the annotate step (worker restart, lifecycle-lock contention with
a concurrent hibernate, a 500) returns an error to the client while the sandbox
keeps running, holding a slot and memory, unknown to the caller. Idempotent
retry creates a second one.

**Fix.** Destroy on annotate failure, or — better — carry `source_type`,
`source_id`, and `metadata` in the create body so there is one commit point.

### C7 — v1 silently drops or rejects fields for snapshot-sourced creates — **Medium** [verified]

`handler.go:193-197`:

```go
if source.Type == "snapshot" {
    path = "/snapshots/" + … + "/fanout"
    legacyBody["count"] = 1
    delete(legacyBody, "ssh_pubkey") // legacy fanout does not provision keys
}
```

- `ssh_public_key` is **silently discarded** — the client gets 201 and an
  unreachable sandbox.
- `resources` is forwarded (`:188-191`) into an endpoint that rejects any
  nonzero `vcpus`/`mem_mib` with 400 (`snapshot.go:668-671`), and v1's own
  validation requires `vcpu >= 1` if `resources` is present
  (`handler.go:726-728`) — so `{source:{type:"snapshot"},resources:{…}}` can
  only ever 400, with a message about fanout.

**Fix.** Either implement key install on the snapshot path (the worker already
has `installSSHKey`, and `finishClone` runs `initializeGuestIdentity` for
independent clones) or return an explicit 400. Reject `resources` in v1
validation for snapshot sources with a message that names the field.

### C8 — Raw ingress leases leak permanently — **Medium** [verified]

`reconcilePendingRaw` (`internal/gateway/raw.go:625-685`) only considers leases
in `pending` or `releasing`:

```go
if (lease.State == "pending" || lease.State == "releasing") &&
    time.Since(lease.UpdatedAt) > 2*time.Minute { candidates = append(…) }
```

An **`active`** lease whose sandbox is gone is never reclaimed. Sandboxes
disappear without going through `handleGatewayDestroy` routinely: the TTL reaper
(`ttl.go:63`), `shutdownAll`'s destroy fallback (`server.go:602`), a
`cleanupExitedMachine` after a VMM crash (`machine_watch.go:65`), or C1/C3
above. Each such death permanently consumes one public port from
`RAW_PORT_MIN..MAX`. `checkHeartbeat` (`raw.go:270-281`) observes the divergence
and only increments a counter and prints.

**Fix.** Reconcile `active` leases against heartbeat-reported `RawRoutes`: a
lease whose sandbox is absent from every heartbeat for N intervals gets
released. The heartbeat already carries exactly the data needed
(`internal/server/heartbeat.go:112-123`).

### C9 — Ready-pool claim does not check that the VM is alive — **Medium** [verified]

`ClaimWarm` (`registry.go:1446-1478`) promotes the oldest `warming` row purely
on database state. `claimWarm` (`internal/server/warm_pool.go:74-89`) adds no
liveness check. If that VM's firecracker died moments earlier, `watchMachine` →
`cleanupExitedMachine` (`machine_watch.go:32-68`) will `MarkStopping` and destroy
the row — after `handleCreate` already returned 201. The client holds an id that
404s immediately.

Probability is low in steady state and non-trivial under memory pressure, where
the kernel OOM-killer picks off VMMs. `CLAUDE.md` notes "the maintainer polls as
well as accepting kicks, so an unexpectedly dead ready VM is replenished" —
replenishment is handled; *handing out a corpse* is not.

**Fix.** In `claimWarm`, verify `s.machines` still holds the id (cheap) and
optionally re-probe `/health` (a millisecond on the warm path); on failure,
destroy and fall through to the next pool entry or the clone path.

### C10 — Warm-pool replenishment hot-loops on persistent failure — **Medium** [verified]

`maintainWarmPool` (`warm_pool.go:91-148`) computes the deficit, launches that
many builds concurrently, and on failure waits **one second**
(`waitWarmRetry`, `:163-173`) before trying again — forever, with no backoff and
no circuit breaker. With `warm_pool_size: 8` and a persistent cause (ENOSPC,
golden deleted, jailer identity pool exhausted, cgroup delegation broken),
that's 8 VM launch attempts per second: rootfs staging, jail construction,
cgroup writes, and a log line each. The host is already unhealthy; this converts
it into disk and CPU thrash plus a log flood that hides the root cause.

**Fix.** Exponential backoff capped at ~30 s, and stop retrying after K
consecutive failures until a kick or a golden change (the
`sandbox_warm_build_failures_total` counter already exists to alert on).

### C11 — Fanout can load a live sandbox's rootfs — **Medium** [inferred]

`handleFanout` stages the snapshot's baked rootfs path only when it's absent:

```go
// internal/server/snapshot.go:711-717
if _, statErr := os.Stat(snap.SourceRootfsPath); statErr != nil { … stage … }
```

`SourceRootfsPath` is the *source sandbox's own* rootfs path. Snapshotting does
not destroy the source, and nothing blocks a fanout while the source runs (only
`restore` is blocked, by the tap/IP unique indexes). So when the source is
alive, every clone's `LoadSnapshot` opens the live sandbox's ext4 image before
`PATCH /drives` relocates it to the clone's CoW copy.

I could not verify what Firecracker does with that descriptor between load and
the drive patch. It is very likely harmless — the guest isn't resumed yet — but
the safety of "N VMMs open the same mounted ext4" rests entirely on that
ordering, and `CLAUDE.md` itself warns "don't share the rootfs between VMs —
ext4 corrupts under concurrent mount".

**Fix.** Always stage to a distinct temporary path and patch from there; never
let a snapshot's baked path resolve to a live sandbox's disk.

### C12 — `cleanupExitedMachine` misses the host-port guard — **Low** [verified]

`machine_watch.go:60-62` calls `RemovePortForwardTo(pm.HostPort, …)` for every
mapping, where `destroyLocked` (`server.go:1176-1180`) and `reconcile`
(`reconcile.go:66-70`) correctly skip `HostPort == 0`. URL-only mappings now
produce `iptables -D … --dport 0` invocations. Harmless, inconsistent.

### C13 — Unrecovered panic path in the gateway — **Low** [verified]

`apiv1.runBatch` dereferences the operation map without a nil check
(`handler.go:517, 548`), while `pruneOperationsLocked` (`:669-675`) deletes any
entry older than 24 h **including running ones**. A batch that outlives its
record nil-derefs in a background goroutine — which `net/http` does not recover
— crashing the gateway process. Practically unreachable (batches finish in
minutes) but worth closing since the cost is one `if op == nil`.

### C14 — `handleDeletePort` can leave row and listener divergent — **Low** [verified]

`ports.go:158-165` deletes the mapping row, then syncs listeners; a `Sync`
failure returns 500 with the row already gone and the listener possibly still
bound. The port is then free in the pool but occupied on the host, so the next
sandbox to draw it fails to bind.

---

## 4. Capacity, scale, and throughput

### X1 — Two different per-VM memory overheads — **High** [verified]

Admission charges 156 MiB per VM:

```go
// internal/server/server.go:232-235
const fcOverheadMIB = 156
```

The jailer's per-VM cgroup allows 512 MiB:

```go
// internal/vm/jailer.go:865
memoryMax := (req.MemMIB + cfg.MemoryOverheadMIB) << 20   // default 512
```

(`JailerMemoryOverheadMIB` default 512 at `internal/config/config.go:250-252`
and `internal/vm/jailer.go:19`.)

`deploy-job.sh` sizes both `mem_budget_mib` and the Nomad task cgroup as
`SLOTS × MEM_PER_SLOT_MIB` (1180 = 1024 + 156). So a host admitted to exactly
its budget hosts VMs whose per-VM cgroup allowances sum to `SLOTS × 1536` —
about **30 % above the parent task's `memory.max`**. The per-VM caps prevent one
guest from ballooning, but they do not prevent the aggregate from exceeding the
parent. If real VMM overhead ever lands between 156 and 512 MiB (large guests,
UFFD handler mappings, snapshot writes), the **parent** cgroup OOMs — and the
parent contains `serve`, so every VM on the host dies at once.

I have not measured actual overhead, so the trigger is unverified; the accounting
inconsistency is not.

**Fix.** Make one number authoritative. Charge `JailerMemoryOverheadMIB` in
admission (`fcOverheadMIB` becomes config-derived), or lower the cgroup
allowance to match. Independently: alert on parent-cgroup `memory.events`
(`high`/`max`), which today would be the only warning.

### X2 — One SQLite connection serves control plane and data plane — **High** [verified]

`registry.go:336` — `db.SetMaxOpenConns(1)`. The reasoning in the comment is
sound (avoiding SQLITE_BUSY on lock upgrade in `Create`'s read-then-insert), and
the conclusion — "they're sub-millisecond … so this isn't a throughput concern"
— was true before the data plane started using the registry. It now does:

- **every accepted forwarded-port connection** → `dialGuest` → `ensureRunning`
  → `reg.Get` (`portproxy.go:214-229`);
- **every `CONNECT` tunnel** (the public edge's data path) → `portIsExposed` =
  2 queries (`connect.go:81-95`);
- **every heartbeat**, every 5 s per host: `RoutedCapacity` + `WarmCount` +
  `ListSnapshots` + **one `Ports()` query per routed sandbox**
  (`heartbeat.go:112-123`) — O(N) round trips through a single connection;
- **every metrics scrape** → an 8-subquery aggregate (`registry.go:265-294`);
- the idle reaper's `List` every 30 s (`hibernate.go:285`).

All of it queues behind `Create`/`Wake` transactions that hold the connection
across `loadUsed` (two full scans) plus an insert. At a few hundred sandboxes
with real ingress traffic, this is the host's ceiling — and the failure mode is
the bad one: **data-plane load slows down creates**, so the gateway sees
capacity pushback caused by traffic rather than capacity.

**Fix.** Three independent wins:
1. A second, read-only `*sql.DB` with `SetMaxOpenConns(N)` — WAL already
   supports concurrent readers, and every path above is a pure read.
2. Collapse the heartbeat's per-sandbox `Ports()` into one `JOIN`.
3. Cache guest IP for the proxy path, invalidated on wake (the current comment
   correctly forbids caching *across* a wake, not caching at all).

### X3 — Unbounded connection fan-in on the worker data plane — **High** [verified]

`portForwarder.serve` (`portproxy.go:145-153`) accepts in a tight loop and
spawns `f.handle` per connection. Each handler holds a 90 s dial budget
(`:32, 163`), takes an activity pin, and hits the registry. There is **no
per-sandbox cap, no per-host cap, and no rate limit**. `handleConnectPort`
(`connect.go`) is the same shape.

The edge, by contrast, has both: `MaxConnections` (100 000 default) with a
semaphore (`edge.go:138-140, 256, 335-346`) and a per-source rate limiter
(`:250-255`). The worker — which is what the edge dials, and what forwarded host
ports expose directly — has neither. A single client can spawn tens of thousands
of goroutines and saturate the single DB connection (X2), degrading everything
on the host including creates.

**Fix.** Per-sandbox and per-host accept semaphores, a short accept-side rate
limit, and a much shorter dial budget when the sandbox is already running (90 s
is sized for the worst-case clone-path wake and applied unconditionally).

### X4 — Inconsistent partial-result semantics on the two list endpoints — **Medium** [verified]

`GET /sandboxes` fails closed — one unreachable candidate host makes the whole
call 502 (`gateway.go:1231-1251`). The rationale is right (a partial list looked
like sandbox loss to real clients). But at fleet scale one wedged worker makes
`list` unusable for every client, with no `partial=true` escape and no cached
fallback, and the 5 s per-host timeout (`:1212`) means a single slow host adds
5 s to every list.

`GET /snapshots` does the **opposite**: it skips failed hosts and returns 200
with a partial list (`gateway/snapshots.go:48-59`). So a snapshot can silently
vanish from a listing, and a client may conclude it's gone and re-create.

**Fix.** One semantic for both, with partiality in the response
(`"incomplete_hosts": [...]`) so clients can decide. Consider a short-TTL cache
so a wedged host degrades to stale rather than fatal.

### X5 — v1 list/get costs scale with the fleet, and pagination is unstable — **Medium** [verified]

- `GET /v1/snapshots/{id}` calls `GET /snapshots` (whole-fleet scatter-gather)
  and linear-scans for one id (`handler.go:380-397`).
- `GET /v1/sandboxes` fetches the entire fleet inventory, filters, sorts, then
  slices (`handler.go:222-255`). Page 10 costs the same as page 1.
- `paginate` (`:789-813`) is offset-based over a list re-assembled per request,
  so a create or delete between pages **duplicates or skips** rows. The cursor
  is an offset plus a hash of the offset (`httpapi.go:224-245`) — opaque, but
  not stable.

**Fix.** Route `GET /v1/snapshots/{id}` through `snapRoute` to one host.
Paginate on a stable key (`created_at, id`) rather than an offset.

### X6 — Idempotency store is O(n) per request, process-local, and mandatory — **Medium** [verified]

`httpapi.Store.acquire` (`:186-201`) sweeps the **entire** map for expiry on
every wrapped request, under one mutex, with a 24 h default TTL
(`handler.go:37`). Entries retain the full response body (`:203-214`). At
realistic create volume that's a five- or six-figure map scanned per request.

Worse, it's per-process: `POST /v1/sandboxes` **requires** an `Idempotency-Key`
(400 without, `httpapi.go:141-145`), but the guarantee evaporates on gateway
restart and does not exist across replicas — so clients pay the API cost for a
promise the system can't keep. `Wrap` also reads the body through
`io.LimitReader(r.Body, 2<<20)` (`:150`), which **silently truncates** a larger
body instead of rejecting it.

**Fix.** Lazy per-key expiry (or a heap), store a body hash + status rather than
bodies for large responses, return 413 on oversize, and either back the store
with shared state or document the guarantee's true scope.

### X7 — Heartbeat payload and gateway critical section grow with inventory — **Medium** [verified]

Each heartbeat carries every routed sandbox id and every non-golden snapshot id
(`heartbeat.go:101-132`), every 5 s, per host. The gateway then rebuilds that
host's whole `route` and `snapRoute` under the **write** lock — the same lock
every proxied request takes to read (`gateway.go:529-561`, `:1400-1407`). With
thousands of sandboxes and snapshots per host this becomes steady multi-MB/s of
JSON plus a lengthening exclusive section on the gateway's hottest lock.

**Fix.** Send deltas with a generation counter and periodic full
reconciliation; move route rebuilds off the request-serving lock (copy-on-write,
as `rawAllocator` already does — `raw.go:82-100, 393-399`).

### X8 — Raw port allocation serializes fleet-wide across network I/O — **Medium** [verified]

`rawAllocator.allocate` (`raw.go:178-268`) holds `a.mu` across two GCS CAS
writes, the worker `assign()` round trip, and jittered backoff. The read path
was correctly moved to a lock-free snapshot in `8425a0c`, but the *write* path
still means raw exposures are globally serialized at a few per second, and each
one rewrites the entire lease index object.

**Fix.** Per-port or sharded locks; a lease index sharded by port range.

### X9 — Worker→guest HTTP client uses Go defaults — **Medium** [verified]

```go
// internal/server/proxy.go:22
var agentClient = &http.Client{}
```

That's `http.DefaultTransport`: `MaxIdleConnsPerHost: 2`, `MaxIdleConns: 100`.
Every guest IP is a distinct host, so:

- more than 2 concurrent exec/file requests to one sandbox force fresh TCP
  handshakes (and accumulate TIME_WAIT);
- with >100 sandboxes on a host, idle connections are evicted continuously, so
  nearly every request pays a handshake.

`internal/client/client.go:51-59` already documents and fixes exactly this for
the gateway→worker path (`MaxIdleConnsPerHost: 64`). The worker→guest path
never got the same treatment. It's a per-request latency tax on the hottest
API in the product.

### X10 — Autoscaler demand counts hibernated sandboxes — **Medium** [verified]

`evaluateDirectScaleOut` (`gateway.go:1000-1021`):

```go
occupied += h.committed() + h.hibernated
…
demand := ceilDiv(occupied+queued+g.directHeadroom, g.directSlotsPerHost)
```

Hibernated sandboxes hold no slot — that is the entire point. Including them
means that once *any* queue forms, desired capacity is computed from total
inventory rather than live demand. A fleet with 1 000 frozen sandboxes and a
brief queue requests workers for all 1 000. Defensible as headroom for a wake
storm; a real cost risk as hibernation adoption grows, and it interacts badly
with the `max_over_time(...[15m])` replay already documented in `CLAUDE.md`.

**Fix.** Separate terms: live demand drives scale-out; hibernated inventory
drives a *floor*, weighted by an observed wake rate.

### X11 — No orphan-artifact garbage collection — **Low** [verified]

`reconcile` sweeps only `hib-lineage-*` directories (`reconcile.go:24-34`).
Never reclaimed:

- snapshot directories from attempts that crashed between `SnapshotPaths` and
  `CreateSnapshot` (`snapshot.go:193, 294`);
- `mem.full.bin` materializations written beside every diff snapshot
  (`snapshot.go:363`) and hibernation (`hibernate.go:92`) — these roughly double
  a diff snapshot's footprint and live until the snapshot row is deleted;
- GCS objects whose `deleteHibernationObjects` pass failed
  (`hib_durable.go:297-315`) — logged, never retried.

Monotonic storage growth across the lifetime of a host.

### X12 — Unbounded negative cache — **Low** [verified]

`g.notFound` (`gateway.go:217`) is a `sync.Map` pruned only when an entry is hit
(`adopt.go:44-47`). Requests for random ids — trivially generated, and reachable
from the public edge — grow it without limit.

---

## 5. Latency and user experience

### L1 — Unknown ids are expensive, and the path is publicly reachable — **High** [verified]

A route miss in `handleProxyByID` (`gateway.go:1409-1422`) calls
`resolveViaAdopt` → `adoptElsewhere`, which tries up to 3 hosts with
`POST /adopt` under a **5-minute** timeout (`adopt.go:23, 75, 82-111`). So:

- a typo'd sandbox id can block for minutes instead of returning 404;
- a client polling right after a legitimate delete pays it (the negative cache
  is only 5 s, `adopt.go:28`);
- `GET /route/{id}` (`route.go:34-43`) and `GET /raw-route/{port}`
  (`raw.go:515`) take the same path — so **unauthenticated public HTTPS traffic
  to any `<port>-<random>.<domain>` hostname** triggers a cross-host adopt
  fan-out with worker-side GCS lookups. The edge's per-source rate limit
  (`edge.go:250`, `FirstHitRate` default 20) is the only brake, and it's
  per-source.

**Fix.** Split "is this id known anywhere" from "adopt it". Cheap negative
answer first (a bounded, longer negative cache keyed by "no durable record"),
adopt only on a positive durable-record probe, and cap adopt-on-miss latency to
a few seconds for interactive paths. Rate-limit adopt dispatch fleet-wide.

### L2 — Create queueing is unfair with no feedback — **Medium** [verified]

`awaitHost` (`gateway.go:907-951`) has every waiter race a 250 ms tick plus a
broadcast channel; whoever wins `reserveHost` first gets the slot. There is no
FIFO. Under sustained saturation an unlucky create can wait the full
`--queue-wait` (**240 s** default, `cmd/sandbox/gateway.go:66`) while
later-arriving creates are served. The client learns nothing until it either
succeeds or gets a 503 four minutes later — no queue position, no ETA, no early
"this will not be served" signal.

**Fix.** FIFO admission (a ticket channel), and surface position/estimate in a
header so SDKs can show progress or bail early.

### L3 — Per-host create admission has the same shape — **Medium** [verified]

`acquireCreate` (`server.go:770-777`) blocks on the semaphore with only the
request context as an escape, and a client timeout produces a non-standard
`499` (`server.go:685`, `snapshot.go:440`). No queue bound, no depth metric on
the worker side (`sandbox_create_inflight` reports holders, not waiters).

### L4 — Snapshot pauses the guest across the rootfs freeze — **Medium** [verified]

`snapshot.go:247` copies the rootfs while the VM is paused, deliberately (the
disk must match the memory image). `CopyFileSparse` → `CloneFile` is an instant
reflink on XFS/btrfs, and a **full multi-GB copy on anything else**
(`provisioner.go:270-284`). On ext4 the user's sandbox is frozen for seconds per
snapshot, with no warning, no progress reporting, and no pre-flight check that
the snapshot directory can actually reflink. `doctor` checks a lot; it doesn't
check this.

**Fix.** Probe reflink support at startup, expose it in `GET /info`, and warn
loudly (or refuse snapshots) when a pause-time copy would be unbounded.

### L5 — One lock serializes metadata operations behind VM lifecycle — **Medium** [verified]

`wakeLock(id)` is taken by `destroy`, `hibernate`, `wake`, `snapshot`,
`createCold`, **and** `rename` (`server.go:971`), `setTimeout` (`ttl.go:33`),
`public-fields` (`server.go:1061`), `expose-port` (`ports.go:42`),
`delete-port` (`ports.go:149`), `set-public-port` (`ports.go:195`).

Correct — but it means a rename issued while the sandbox is waking blocks for
the whole wake (up to 30 s, `hibernate.go:675`), and one issued during a cold
create blocks up to 60 s (`server.go:853`). Pure metadata writes should not
queue behind VMM operations.

**Fix.** Split the lock: a lifecycle lock (VM state) and a metadata lock (row
fields), with metadata writes using conditional updates rather than
read-modify-write under the lifecycle lock.

### L6 — One idle connection defeats hibernation indefinitely — **Medium** [verified]

`portForwarder.handle` brackets the *entire* connection lifetime with an
activity pin (`portproxy.go:158-161`), and `handleShellProxy` does the same for
a shell (`proxy.go:100-101`). A browser keep-alive, an abandoned `sandbox
shell`, or a monitoring probe that holds a socket open therefore pins the
sandbox running forever — disabling the primary density and cost lever, with no
maximum session duration and no idle-bytes timeout.

**Fix.** Distinguish "connection open" from "connection active": pin on bytes
transferred within a window, not on socket existence. Add a max session duration
for shells and a configurable idle-bytes timeout for forwarded connections.

### L7 — Agent readiness polling is hot and opaque — **Low** [verified]

`waitForAgent` (`proxy.go:286-309`) polls every 25 ms with a 100 ms per-attempt
timeout for up to 60 s — up to ~2 400 HTTP attempts against a booting guest.
The tight poll is a deliberate (and correct) fix for ARP-retransmit stalls; the
cost shows on the failure path, where the caller learns nothing for 60 s and
then gets one generic error. No metric distinguishes "agent slow" from "agent
never came up".

### L8 — Fanout reports partial success as complete success — **Low** [verified]

`snapshot.go:773-788` returns 201 with whatever clones survived and no per-item
errors: ask for 10, get 3, still 201. `POST /v1/sandbox-batches` models this
correctly with per-index results and a `partially_succeeded` status
(`handler.go:141-157, 550-557`); the legacy endpoint the SDK's `createMany`
ultimately reaches does not.

---

## 6. Operability and API contract

### O1 — Misleading status codes on several failure paths — **Medium** [verified]

- `DELETE /snapshots/{id}` defaults to **404** for every non-`ErrSnapshotInUse`
  error (`snapshot.go:990-999`). A GCS outage in
  `deleteSnapshotLocked`'s marker delete (`:1030`) therefore reports "not
  found" — the client concludes success-by-absence and stops retrying, while
  the row and objects survive. This is the single most misleading code in the
  API.
- `POST /sandboxes/{id}/hibernate` maps every failure to 409
  (`hibernate.go:741-744`), including internal snapshot failures.
- The gateway's `DELETE /sandboxes/{id}` returns **204 for a sandbox that never
  existed** (`raw.go:566-582`) where the worker returns 404 — so delete
  semantics depend on whether you're talking to a gateway or a worker.

### O2 — Two error shapes and two validation policies on one server — **Medium** [verified]

`{"error": "..."}` for legacy routes (`server.go:1206`, `gateway.go:1663`) vs
RFC 9457 `application/problem+json` for `/v1/*` (`httpapi.go:72-89`). And
legacy create silently ignores unknown JSON fields (`server.go:642-653`) while
v1 rejects them (`handler.go:815-831`) — so a misspelled `mem_mb` is a silent
default on one path and a 400 on the other. Since both are documented and in
use, this is a support burden rather than a bug.

### O3 — `mem_budget_mib` default is wrong inside a cgroup — **Medium** [verified]

```go
// internal/server/server.go:265-270
if budget == 0 { if total := hostTotalMemMIB(); total > 2048 { budget = total - 2048 } }
```

`hostTotalMemMIB` reads `/proc/meminfo` (`:1322-1340`), which inside a Nomad
task reports the **machine** total. The comment says fleet hosts must set it
explicitly — but nothing enforces that, and the consequence of forgetting is
over-admission until the task cgroup OOM-kills every VM. The jailer path already
reads `memory.max` from the task cgroup (`jailer.go:118`), so the correct value
is available.

**Fix.** When running in a cgroup with a finite `memory.max`, derive the budget
from it and log the source. Refuse to start a fleet worker (`--gateway` set)
with an unset budget.

### O4 — Batch operations vanish on restart — **Medium** [verified]

`Handler.operations` is an in-memory map (`handler.go:31, 501-513`). A gateway
restart loses every operation record while the sandboxes it created keep
running; clients polling `GET /v1/operations/{id}` get 404 with no way to
reconcile what was created. The 202 + Location contract implies durability it
doesn't have.

### O5 — No cross-field validation of pool geometry — **Low** [verified]

`Pools.ipPoolSize` (`registry.go:190-200`) computes `int(maxIP-minIP) + 1` on
uint32s: a config with `GuestIPMin > GuestIPMax` **underflows to a huge
positive**, so `Slots()` reports fictitious capacity and the host advertises it
to the gateway. Nothing checks the IP pool fits `guest_subnet_bits`, that
`TapMax` matches the IP pool, or that the port range is sane. `deploy-job.sh`
validates some of this for the fleet; a hand-written config gets none of it, and
`config.Load` is otherwise strict (`DisallowUnknownFields`, jailer limit
checks).

### O6 — Unstructured logs, no levels, no sampling — **Low** [verified]

One `log.Printf` per request (`httpapi.go:48`) plus `fmt.Fprintf(os.Stderr, …)`
throughout, at no level and with no sampling. Per-clone phase lines
(`snapshot.go:961-966`) are excellent for debugging and noisy at fleet scale.
Metrics coverage is genuinely good; logs are the weak half of the observability
story.

### O7 — A worker can claim any sandbox id — **Low** [verified]

`handleRegister` accepts `hb.SandboxIDs` unconditionally and writes them into
the routing table (`gateway.go:542-545`). Any holder of the worker-control token
can hijack routing for arbitrary ids. Consistent with the current "workers are
trusted" model, and only interesting once S1 is fixed — at which point the
worker token stops being obtainable by clients and this becomes a genuine
boundary worth defending (sign heartbeats, or cross-check against the gateway's
own reservation record).

---

## 7. Component-by-component notes

**`internal/registry`** — the strongest module. Explicit six-state machine,
atomic pool allocation inside transactions, partial unique indexes as the
correctness backbone, memory admission in the same transaction as the insert,
`RoutedCapacity` deliberately single-snapshot to avoid mismatched counts. Issues
are C1 (stale identity on hibernated rows entering the index set), C5
(non-transactional snapshot delete), X2 (single connection), O5 (pool geometry
validation). The `RoutedCapacity` occupancy loop (`:969-992`) is correct but
counts in two places and would benefit from a single explicit
`holdsCapacity(status)` predicate.

**`internal/server`** — the largest surface. Lifecycle correctness is careful:
`vmCtx` decoupling, `keyedMutexes` with reference counting (a real fix, not a
band-aid), pre-VM rollback on every failure path, `MarkRunning` gating
visibility until every readiness gate passes. Weak points: C1/C2/C3 (lifecycle
vs durability coupling), C4 (fanout bounds), C9/C10 (warm pool), L5 (one lock
for lifecycle and metadata), X2/X3/X9 (data-plane cost), X11 (artifact GC).

**`internal/gateway`** — impressive placement logic: reservations visible to
concurrent picks, capacity-class failover with penalties, bin-packing to make
scale-in safe, level-triggered scale-out with a grow-only watermark, and the
route pinning fixed in `8425a0c`. Its problems are almost all at the edges:
S1/S2 (route/credential exposure), X4 (list semantics), X7 (heartbeat cost),
X10 (demand accounting), X12, L1/L2, C8 (raw lease reclamation), C13.

**`internal/vm` (+ jailer)** — the isolation work is thorough and clearly
written: trusted-path validation walking to `/`, version-matched
jailer/firecracker, identity reservation via `O_EXCL` lock files with startup
reclamation (`jailer_reconcile_linux.go:86-102` — good, and easy to miss),
cgroup delegation that fails closed on a foreign process, bounded VMM logs with
age+count pruning. Findings: X1 (overhead mismatch), and a note that `cpu.max =
vcpus × period` (`jailer.go:866`) is a hard cap shared with the VMM's own
threads, so a 2-vCPU guest cannot actually use 2 full cores — worth measuring
before advertising vCPU counts.

**`cmd/sandboxd`** — small and careful: process-group kill, `WaitDelay` for
background children, capped output, correct `setfsuid` scoping with verified
restore, resolv.conf materialization (a genuinely subtle bug well fixed). Main
finding is S3 (no authentication) and L6 (shells pin forever). No read/write
timeouts on the listener (`main.go:61`).

**`services/sandbox-edge`** — the best-behaved network component: connection
semaphore, per-source rate limit, single-flight route resolution with negative
caching, `NextProtos: http/1.1` to prevent connection coalescing misrouting
(a sharp detail), cert hot-reload with expiry gauge, drain on shutdown. It
inherits S1 (it's handed a worker token) and amplifies L1 (novel hostnames →
adopt).

**`internal/apiv1`** — the weakest layer, because it re-implements a control
plane in terms of in-process calls to another one. C6 (non-atomic create), C7
(dropped/rejected fields), C13 (panic path), X5 (fleet-wide reads per request),
X6 (idempotency), O4 (volatile operations). The `responseRecorder`
(`handler.go:887-901`) implements neither `Flusher` nor `Hijacker`, so no
streaming or upgrade route can ever be adapted through it — fine today, a trap
tomorrow.

**`internal/httpapi`** — request IDs, RFC 9457 problems, and `statusWriter` with
`Unwrap` so `ResponseController` reaches `Flusher`/`Hijacker` (the fix for a
regression that broke every WebSocket error — worth keeping a test on). X6 is
the finding.

**`internal/wsutil`, `internal/management`, `internal/gcsblob`,
`internal/provisioner`** — no new findings beyond S3/S5. The subprotocol-auth
design and its three documented constraints are correct and unusually
well-explained; `Reject`'s use of `NewResponseController(w).Hijack()` is exactly
right and should stay covered by a test.

---

## 8. What is already solid (don't regress it)

Worth writing down so a refactor doesn't undo it:

- `vmCtx` decoupled from the serve context — without it, SIGTERM kills VMs
  before `shutdownAll` can freeze them (`server.go:297-305`).
- Partial unique indexes as the pool-allocation invariant, allocation inside
  the insert transaction (`registry.go:378-379, 642-690`).
- Memory admission inside the same transaction as the insert, including on
  `Wake` (`registry.go:1597-1619, 907`).
- `keyedMutexes` reference counting — a correct fix to an unbounded map
  (`keyedlock.go`).
- `diffBase`/`hibLineage` explicitly separate from `BaseSnapshotID`, with the
  reasoning for why the latter must not be trusted (`server.go:157-167`).
- Startup reconciliation that verifies `/proc/<pid>/comm` before killing
  (`reconcile.go:89-98`) and jailer reconciliation that refuses to release an
  unverified live jail (`jailer_reconcile_linux.go:58-65`).
- WebSocket errors as post-handshake close frames — the only form browsers
  surface (`wsutil.Reject`, and the `ResponseController` hijack).
- Boot-phase instrumentation as absolute timestamps, so a 10 s scrape recovers
  millisecond boundaries (`bootphase.go`).
- Copy-on-write read snapshot in `rawAllocator` — the right pattern, and the one
  X7 should copy.

---

## 9. Prioritized remediation plan

**P0 — before this is a service others depend on**

| # | Item | Rough effort |
| --- | --- | --- |
| S1 | Separate edge credential; stop returning worker tokens from `/route`, `/raw-route` | 1–2 d (+ token rotation) |
| S2 | Move `/worker-release`, `/hosts`, `/drain` behind the worker/operator domain | 2 h |
| C1 | Clear tap/IP on hibernate; reorder `destroyLocked`; regression test | 0.5 d |
| C3 | Decouple durability cleanup from hibernate/destroy | 0.5 d |
| C2 | Restore identity in `rollbackWake` | 2 h |
| C4 | Bound fanout `count`; route through `createSem`; bound phase 2 | 3 h |
| X1 | Single authoritative per-VM overhead; alert on parent-cgroup pressure | 0.5 d |

**P1 — before meaningful scale**

X2 (read pool + heartbeat JOIN + proxy IP cache), X3 (data-plane limits),
S3 (authenticate sandboxd + re-assert isolation + metric), L1 (cheap negative
answer before adopt), C8 (reclaim active raw leases), X9 (tune `agentClient`),
O3 (cgroup-aware memory budget), C6 (atomic v1 create), O1 (status codes),
C9/C10 (warm-pool liveness + backoff).

**P2 — quality and cost**

L2/L3 (FIFO queueing + feedback), L5 (split metadata from lifecycle lock),
L6 (activity ≠ open socket), X4 (one list semantic), X5/X6 (v1 read paths and
idempotency), X7 (delta heartbeats), X10 (demand accounting), X11 (artifact GC),
S4/S5/S6 (secure-by-default config), O2/O4/O5/O6, C5, C7, C11–C14, L4, L7, L8.

---

## 10. Test gaps

The existing suites are genuinely good where they exist: gateway placement /
queue / metrics / route pinning, registry hibernate-wake state machine, port
proxy forwarding + wake-on-connect + lifecycle, `wsutil` close codes, jailer
prerequisites and reconciliation, boot phases, plus the TypeScript mock-server
suite and the `tests/` fleet e2e. Nothing currently covers:

1. Destroy and TTL-reap of a hibernated sandbox whose tap/IP were reassigned (C1).
2. `rollbackWake` identity restoration, then a successful second wake (C2).
3. Hibernate and `shutdownAll` against a blob store that returns errors (C3) —
   assert the sandbox is frozen or left intact, never destroyed.
4. Fanout with `count` above the cap, and fanout concurrent with creates
   competing for `createSem` (C4).
5. Reclamation of an `active` raw lease whose sandbox vanished (C8).
6. Claiming a warm VM whose machine has already exited (C9).
7. Warm-pool backoff under a permanently failing builder (C10).
8. A negative assertion that `/route` and `/raw-route` reject the client
   credential (S1/S2) — the kind of test that keeps a fix fixed.

---

## Appendix A — reproduction probes

Both probes are `internal/registry` package tests. They **fail today**, which is
the point: they are the regression tests for C1 and C2 once the fixes land. Drop
either into `internal/registry/` and run
`go test ./internal/registry/ -run TestAuditProbe -v`.

### A.1 — C1: destroying a hibernated sandbox whose tap/IP were reused

Observed on `8425a0c`:

```
A(hibernated) tap=fc0 ip=172.16.0.10 ; B(running) tap=fc0 ip=172.16.0.10
REPRODUCED C1: destroying hibernated A fails permanently: constraint failed:
  UNIQUE constraint failed: sandboxes.guest_ip (2067)
```

`TapMax: 1` forces the reuse that the pickers' soft-avoid pass normally dodges;
on a real host the same state arrives whenever occupancy is high enough that the
first pass finds nothing free.

```go
package registry

import (
	"context"
	"path/filepath"
	"testing"
)

// Probe for audit finding C1: a hibernated row keeps its stale tap/IP, so once
// the pool is tight enough that a running sandbox reuses them, MarkStopping
// (which destroy() calls) moves the hibernated row into uniq_tap_running and
// collides.
func TestAuditProbeHibernatedDestroyCollision(t *testing.T) {
	// One tap / one IP: guarantees the reuse the soft-avoid pass normally dodges.
	pools := Pools{TapPrefix: "fc", TapMax: 1, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.10", PortMin: 5200, PortMax: 5200}
	r, err := Open(filepath.Join(t.TempDir(), "r.db"), pools)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	a, err := r.Create(ctx, "A", "", "/tmp/a.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Hibernate(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	b, err := r.Create(ctx, "B", "", "/tmp/b.ext4", nil, "", 0, 0, 0)
	if err != nil {
		t.Fatalf("create B after hibernating A: %v", err)
	}
	t.Logf("A(hibernated) tap=%s ip=%s ; B(running) tap=%s ip=%s", a.TapDevice, a.GuestIP, b.TapDevice, b.GuestIP)

	// This is what destroy()/the TTL reaper does first.
	if err := r.MarkStopping(ctx, "A"); err != nil {
		t.Fatalf("REPRODUCED C1: destroying hibernated A fails permanently: %v", err)
	}
	t.Log("MarkStopping succeeded — C1 does not reproduce")
}
```

### A.2 — C2: identity reallocated by a failed wake is never restored

Observed on `8425a0c`:

```
Wake reassigned A: 172.16.0.10 -> 172.16.0.11 (clone path)
REPRODUCED C2: after a failed wake the row says 172.16.0.11 but the frozen
  memory has 172.16.0.10; the next wake sees 172.16.0.11 free, takes the
  same-identity path, and boots a guest whose real IP is 172.16.0.10
```

This proves the registry half — the reallocated identity survives the rollback.
The consequence (the next wake loading a snapshot whose baked IP no longer
matches the row, so `waitForAgent` polls an address nothing answers on) follows
from `hibernate.go:558-561, 675` and would need a live VM to observe end to end.

```go
package registry

import (
	"context"
	"path/filepath"
	"testing"
)

// Probe for audit finding C2: Wake persists a NEW tap/IP when the old pair is
// taken. rollbackWake (server) only flips status back via Hibernate, so the
// reallocated identity survives a failed wake while the frozen memory still has
// the old one baked in.
func TestAuditProbeWakeIdentityNotRestored(t *testing.T) {
	pools := Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201}
	r, err := Open(filepath.Join(t.TempDir(), "r.db"), pools)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	a, _ := r.Create(ctx, "A", "", "/tmp/a.ext4", nil, "", 0, 0, 0)
	frozenIP := a.GuestIP // identity baked into A's hibernation memory image
	if err := r.Hibernate(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	// Something takes A's old identity while it sleeps.
	if _, err := r.CreateRestore(ctx, "B", "", "/tmp/b.ext4", a.TapDevice, a.GuestIP, nil, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	woken, same, err := r.Wake(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("expected the clone path (same=false)")
	}
	t.Logf("Wake reassigned A: %s -> %s (clone path)", frozenIP, woken.GuestIP)

	// Simulate the wake failing: rollbackWake's only registry action.
	if err := r.Hibernate(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Get(ctx, "A")
	if after.GuestIP != frozenIP {
		t.Fatalf("REPRODUCED C2: after a failed wake the row says %s but the frozen memory has %s; "+
			"the next wake sees %s free, takes the same-identity path, and boots a guest whose real IP is %s",
			after.GuestIP, frozenIP, after.GuestIP, frozenIP)
	}
}
```
