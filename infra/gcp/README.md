# GCP infra (gcloud)

Plain `gcloud` scripts for the sandbox fleet on GCP, all in **Mumbai
(`asia-south1`)**. Two paths:

1. **Autoscaling fleet (production)** — a control VM + a Managed Instance Group
   of workers, resized automatically by the Nomad Autoscaler. See
   **[Autoscaling fleet](#autoscaling-fleet)** below.
2. **Static debug VMs** — hand-named throwaway VMs (`vms.sh` + `fleet-deploy.sh`).
   Good for one-off debugging; documented under
   **[Static debug VMs](#static-debug-vms)**.

---

## Autoscaling fleet

The elastic fleet: `sandbox gateway` places sandboxes and exposes a `/metrics`
scaling signal; Prometheus turns it into `sandbox:workers_desired`; the Nomad
Autoscaler resizes a worker MIG to match. Workers run `sandbox serve` as a Nomad
**system job**, so a newly-booted worker starts serving within seconds of
joining the cluster.

For the latency model, comparison with Modal's published architecture, and the
next implementation steps (level-triggered direct scaling, tap recycling,
staged create concurrency, and prepared microVM pools), see
[Autoscaling and burst-start latency](../../docs/autoscaling-latency.md).

### Public ingress edge

Public ingress is a separate deployable service, not a worker or control-plane
role. Configure `INGRESS_BUCKET`, `EDGE_DOMAIN`, the raw port range, and either
initial certificate files or `EDGE_ACME_EMAIL` in `config.env`, then:

```bash
make -C ../.. gcs-release
./control.sh up
./control.sh deploy
./edge.sh init
./edge.sh up
./edge.sh status
```

Create wildcard DNS for `*.${EDGE_DOMAIN}` and the raw hostname at the regional
IP printed by `edge.sh status`. `edge.sh roll` creates an immutable instance
template and replaces replicas with zero configured unavailability; the backend
drains established connections for five minutes.

The gateway alone has write access to `gs://${INGRESS_BUCKET}`. Edge VMs only
read the release artifact, certificate/key, and gateway token. Prometheus
discovers replicas by the `sandbox-edge` GCE network tag, and the existing
Grafana dashboard includes edge health, wake latency, errors, and raw leases.

**Topology** (all in `asia-south1-a`, VPC-internal):

- **`sandbox-control`** — one small non-spot VM: Nomad server + `sandbox gateway`
  (:9090) + Prometheus (:9091) + nomad-autoscaler. Reserved static internal IP;
  Tailscale for laptop access + a **subnet router** advertising the VPC subnet
  (so the laptop can reach sandbox forwarded ports on the VPC-internal workers,
  which are *not* on the tailnet).
- **`sandbox-workers`** — a MIG of `n2-standard-8` Firecracker hosts built from
  the baked `sandbox-worker` image. **Non-spot by default** (running sandboxes
  must not be preempted); set `WORKER_SPOT=true` for a cheap, evictable dev
  fleet. The autoscaler owns the MIG size between `MIG_MIN` and `MIG_MAX`.

**Bring-up** (from `infra/gcp`, after `cp config.env.example config.env` + edit):

```bash
# one-time: buckets, SAs, image
./snapshot-store.sh init            # snapshot bucket + sandbox-fleet-sa (also used by workers)
./mig.sh init                        # release bucket + grant SA read + firewall check
./bake-image.sh                      # bake the sandbox-worker image (~8-10 min)

# control plane
./control.sh up                      # SA + static IP + create the control VM
#   approve the advertised subnet route in the Tailscale admin console (one-time)
./control.sh deploy                  # gateway + nomad server + prometheus + autoscaler

# workers + the serve job
make -C ../.. gcs-release             # build + upload binaries to gs://$RELEASE_BUCKET/releases/<sha>/
./mig.sh up                          # instance template + MIG at MIG_MIN
./deploy-job.sh                      # submit the sandbox-serve system job to Nomad
```

### Public ingress needs TWO things that no health check covers

Both failed silently for a day after the ingress rollout, and the symptom was
identical in each case: the API hands back a private `10.x` worker IP and never
a URL.

1. **`EDGE_DOMAIN` must be in `config.env`.** It feeds two independent places —
   `edge.sh` bakes it into the edge instance template, and `deploy-job.sh`
   copies it into each worker's `ingress_domain`. A worker with an empty
   `ingress_domain` decorates no exposure with a URL, so `POST /ports` can only
   ever return `host_port`. Nothing errors; you just never see a URL. Check
   with `nomad job inspect sandbox-serve | grep ingress_domain`.

2. **The edge's gateway token must be `GATEWAY_EDGE_TOKEN`, not
   `GATEWAY_TOKEN`.** `edge.sh` seeds the Secret Manager secret with
   `${GATEWAY_EDGE_TOKEN:-${GATEWAY_TOKEN}}`, so a fleet initialised before the
   edge credential domain existed has the *client* token in there. Once the
   gateway gates `/route` on `--edge-token-file`, every ingress request dies as
   `resolve sandbox: 401` — and **`edge.sh roll` does not fix it**, because roll
   only replaces the instance template and never touches the secret. Rotate it
   explicitly, then let the 5-minute `sandbox-edge-secrets.timer` pick it up:

   ```bash
   printf '%s' "$GATEWAY_EDGE_TOKEN" \
     | gcloud secrets versions add sandbox-edge-gateway-token --data-file=-
   ```

   Compare the two stores by hash before assuming they agree — never print them.

**Drive it** from the laptop over Tailscale (token printed by `control.sh status`):

```bash
SANDBOX_API_URL=http://<control-tailnet-ip>:9090 SANDBOX_API_KEY=<gateway-token> \
  tsx ../../sdk/typescript/benchmarks/fleet-bench.ts --count 20
```

**Iterate on the Go binaries** without re-baking the image — use the one-command
rollout, which does all of the below in the right order and verifies it landed:

```bash
./rollout.sh                 # roll HEAD: build, upload, deploy what's stale, wait, smoke-test
./rollout.sh --fast          # rapid dev iteration (see below)
./rollout.sh --dry-run       # print the plan (what's stale) and exit
./rollout.sh --status        # where the fleet is right now, no changes
./rollout.sh <sha>           # roll a previously published release (rollback)
```

**`--fast` for rapid dev iteration.** Measured, the fleet side of a rollout is
already quick — the golden snapshot is *adopted*, not rebuilt, and the ready pool
refills at ~1.5 s per VM in parallel, so a worker is serving again seconds after
its task restarts. The cost is bytes leaving your machine plus control-plane work
a code change cannot affect. So `--fast`:

- builds only `cmd/sandbox`, stripped (`-s -w`): **30.5 MiB → 15 MiB** of egress.
  Go panic tracebacks come from `pclntab`, so stripping costs `delve`, not stack
  traces;
- runs the GCS upload and the gateway push **concurrently** — they don't depend
  on each other, and previously the binary crossed the network twice in series;
- restarts **only the gateway** (`control.sh gateway`, `SECTIONS=gateway`)
  instead of reinstalling nomad-server/prometheus/autoscaler/grafana, which are
  version-pinned and unaffected by a code deploy;
- compiles **once** (the old path ran `build-linux` twice);
- polls convergence every 2 s instead of 10 s, and skips the smoke check that has
  to wait out a heartbeat interval.

It publishes as **`<sha>-dev`** so a stripped dev artifact can never be mistaken
for — or silently satisfy — a real `<sha>` release, and it always rebuilds (the
same `-dev` label is reused across iterations, so trusting a previous upload
would ship stale bytes). Promote to a real release with a plain `./rollout.sh`.

Note it does **not** need to sacrifice snapshots or sandbox data to be fast:
`shutdownAll` hibernates rather than destroys, and freezing the ready pool is
~178 ms per VM, 8-way parallel — not the bottleneck.

It derives what to deploy by comparing each component's **running** release
against the target, so it's idempotent, and it waits on the gateway's host
inventory (release + `release_compatible` + capacity) rather than `nomad alloc
status` — an unwarmed host advertises `slots_free=0` and can't take a create
yet. The smoke test covers REST *and* the WebSocket pty, because those
authenticate and proxy differently; a REST-only check has passed while the
interactive shell was broken fleet-wide. Also `make fleet-rollout` /
`make fleet-status` from the repo root.

The underlying steps, if you need them individually:

```bash
make -C ../.. gcs-release && ./deploy-job.sh   # new sha rolls the system job fleet-wide
```

If Tailscale SSH requires an interactive reauthentication check, point only
the deployment transport at the control VM's normal SSH endpoint:

```bash
CONTROL_SSH_HOST=<control-ssh-ip-or-name> ./deploy-job.sh <release>
```

The gateway/worker control URL remains the VPC-internal
`CONTROL_INTERNAL_IP`; this override changes only `ssh`/`scp`.

That rolls the WORKERS only. If the change touches the gateway — including
adding fields to `client.CreateOpts` (the gateway re-encodes create bodies
through it, so an old gateway silently drops new fields) — also run
`./control.sh deploy` to update the control plane. Since `sandbox` is a single
binary holding both `serve` and `gateway`, that's effectively **any Go change**;
forgetting it leaves a half-deployed fleet. `rollout.sh` exists because this
step is easy to miss — it deploys the gateway first (a new gateway in front of
old workers is the benign direction; the reverse loses fields).

Note `control.sh deploy` compiles the **working tree** and labels it with HEAD's
sha, so it cannot install some other release. Workers can roll to any published
sha; rolling the gateway back means checking that commit out first. `rollout.sh`
refuses up front instead of half-deploying.

**Observe it** in Grafana at the URL printed by `./control.sh status`. The
provisioned **Sandbox Fleet** dashboard separates live operational telemetry
from offline benchmark evidence:

- gateway demand, queue, rejection, desired-worker, and scaling-owner signals
  refresh on the 10 s control-loop scrape;
- per-worker pools, memory, create concurrency, lifecycle, release, and
  readiness phases arrive through the 30 s federation scrape;
- rollout panels compare the gateway's persisted expected worker release with
  releases actually serving, including workers gated from new placement;
- benchmark p50/p95/p99 values are reference text, not live samples. The
  service does not yet export request-duration histograms.

**Scaling knobs** (`config.env`): `MIG_MIN`/`MIG_MAX` (bounds + cost guardrail),
`SLOTS_PER_HOST` (the **single source of truth** for per-host capacity —
`deploy-job.sh` *generates* the pools in `devbox-gcp.json` from it: taps = IPs
= N, ports = 4N so hibernated port-holds and extra exposed ports never bind
capacity, plus `mem_budget_mib = N×1180` so `mem_mib` overrides are admitted
against the host's real memory — a big-mem sandbox consumes multiple slots'
worth of `slots_free` and can never OOM the cgroup; max 200 per the /24 guest
subnet), `HEADROOM_SLOTS` (free slots kept
ahead of demand), `SCALE_DOWN_WINDOW` (how long demand must stay low before
scale-in), `STANDBY_SUSPENDED_SIZE`/`STANDBY_STOPPED_SIZE` (pre-created standby
VMs; the MIG resumes suspended workers before starting stopped workers, then
falls back to fresh create+boot; apply to a live MIG with `./mig.sh standby`), and
`QUEUE_WAIT`/`QUEUE_MAX` (the gateway's create queue — wait must cover standby
start → nomad join → golden-snapshot build → fresh-worker placement quarantine,
up to ~4 min). `deploy-job.sh` generates `placement_delay_sec` as
`STANDBY_INITIAL_DELAY + PLACEMENT_DELAY_HEADROOM_SEC` (180 + 30 seconds by
default). Fresh refill workers register and route immediately but advertise
zero free slots until that Linux boot age, giving the MIG time to suspend them
before they can receive traffic. Linux uptime includes suspended time, so a
resumed standby advertises capacity immediately. The defaults size the
fleet for **1000 concurrent sandboxes**: n2-standard-16 workers × 48 slots ×
MIG_MAX=22. Scale-up is immediate; scale-down waits out the window.

**Standby policy decision:** keep the full standby pool suspended rather than
mixed with stopped workers. Demand is predictable, so replenish suspended
capacity ahead of forecast bursts; preserving initialized worker memory gives
the fastest scale-out. Stopped standby remains supported as a cheaper fallback,
but the production configuration intentionally sets its target to zero.

**Scale-out confirmation is deliberately short-circuited.** The gce-mig target
polls for MIG-wide stability after each resize, and the policy is frozen in
`StateScaling` for that whole window — every evaluation inside it is dropped with
`skipping scaling, target still scaling`. So the confirmation budget is really a
*scale-up blackout*, and the upstream default (15 attempts × 10 s = 150 s) blocked
a second wave of hosts mid-burst while always ending in
`failed to confirm scale out GCE Instance Group: reached retry limit` — with a
standby pool, MIG stability is unreachable by construction, since the pool spends
~190 s replenishing a replacement suspended worker in the background. We don't
need GCE's confirmation (readiness shows up on the gateway heartbeat, measured by
`sandbox_worker_ready_seconds`), so `AUTOSCALER_RETRY_ATTEMPTS` defaults to `3`
(30 s). Failing fast is safe: a failed confirm returns the policy to Idle **without
cooldown**, and the next evaluation no-ops unless demand actually grew, because the
resize already moved the MIG's target size.

This needs **autoscaler ≥ 0.4.8** — older builds ignore `retry_attempts` and keep
the 150 s blackout — so `AUTOSCALER_VERSION` is now `0.5.0`. **`config.env` is
gitignored: bump `AUTOSCALER_VERSION` (and optionally add
`AUTOSCALER_RETRY_ATTEMPTS`) in your live `config.env`, then
`./control.sh install`**; `control-install.sh` compares the installed binary's
version and re-fetches on mismatch, and warns if the pin is too old for the key.
(It previously guarded the download with `command -v nomad-autoscaler ||`, so a
version bump alone silently kept the old binary.)

**Scale-in freezes running sandboxes on the removed host**: server shutdown
hibernates them (diff snapshots — a full host freezes inside the 120 s stop
window), so they come back wakeable if that VM ever starts again (standby-pool
stop/start does exactly this); on a *deleted* instance the frozen state goes
with the disk, and only saved snapshots survive via GCS durability. Bin-pack
placement + the window minimize how often scale-in hits an in-use host.

**Scaling ownership:** Nomad Autoscaler is the sole writer of the production
MIG target. The gateway has an optional direct GCE scale-out capability for
non-Nomad deployments, but `control-install.sh` deliberately does not enable
it. Enabling both writers caused target-size overshoot after churn because each
acted on a different view of in-flight capacity.

**Burst behavior** end to end: a burst first lands on `HEADROOM_SLOTS` of free
capacity; overflow creates wait in the gateway's bounded queue
(`QUEUE_WAIT`/`QUEUE_MAX`) instead of 503ing, and the queue depth itself feeds
`sandbox:workers_desired` — computed from **effective occupancy**
(`slots_used + hibernated`, NOT `slots_total − slots_free`: a warming host
advertises `slots_free=0` as a placement gate while running zero sandboxes, and
`total − free` misread that as a full host, inflating desired by ~one host) — so
the autoscaler scales up
immediately; the MIG resumes suspended standby workers first, then starts
stopped workers, and finally creates fresh VMs when both pools run dry. A fresh
host advertises `slots_free=0` until both its golden snapshot is built and its
boot-age placement quarantine expires, so it is neither boot-stormed with cold
creates nor selected for traffic while the MIG is about to move it into the
standby pool; each host also bounds concurrent bring-ups
(`create_concurrency`; the fleet explicitly uses 24, while the general default
remains 2×cores capped at 16). When warm-up completes the worker immediately
heartbeats its real capacity, and that heartbeat broadcasts a retry to all
gateway waiters. Worker heartbeats also carry the deployed release. Before
submitting a new Nomad job, `deploy-job.sh` persists that release in the
gateway; a resumed worker still running an older allocation keeps its existing
sandbox routes but is forced to `slots_free=0` until Nomad starts the current
allocation. This prevents a burst from landing on a serve process that the
system-job rollout is about to terminate. A create that still hits a stale host
gets failed over to the next host by the gateway (up to 3 attempts) before it
would ever surface an error. Only a burst that outruns queue-wait + MIG_MAX
sees 503s (with Retry-After).

**Teardown:** `./mig.sh down` then `./control.sh down` (the reserved IP, SAs, and
buckets persist — remove with `gcloud` if you're fully done).

**Ops:** `./control.sh status`, `./mig.sh status`, and on the control VM
`nomad job status sandbox-serve` / `nomad node status`.

### Profiling a scale-up (worker readiness)

Autoscale latency splits into a **decision** span and a **readiness**
span (resize → the new host advertises capacity), which dominates. The readiness
span is instrumented per stage and exported on every host's `/metrics`, federated
to Prometheus with a `host` label:

Prometheus scrapes gateway demand every 10 seconds and Nomad Autoscaler submits
both scale-out and scale-in actions. Gateway direct-scale counters must remain
zero on this fleet; a non-zero value means a second MIG writer was accidentally
enabled.

```promql
sandbox_worker_ready_seconds                  # headline: kernel boot -> capacity advertised
sandbox_boot_phase_seconds{phase="..."}       # each phase, seconds from the boot anchor
sandbox_boot_phase_timestamp_seconds{phase=".."}  # the same as absolute unix time
```

Phase order (adjacent gaps are the per-stage costs):

| phase | written by | the gap before it measures |
|---|---|---|
| `kernel_boot` | serve (`/proc/stat` btime) | — (anchor) |
| `startup_script_entered` | startup-worker.sh | GCE boot → startup script |
| `data_disk_ready` | startup-worker.sh | XFS mkfs/mount/growfs |
| `rootfs_staged` | startup-worker.sh | base rootfs copy (≈free on a golden-seeded disk) |
| `nomad_started` | startup-worker.sh | Nomad client start |
| `serve_task_started` | run.sh (Nomad task) | Nomad join + schedule + GCS artifact pull |
| `serve_process_start` | serve | process exec |
| `reconcile_done` | serve | stale-state cleanup |
| `golden_settled` | serve | golden **adopt** (fast) vs **cold build** (slow) |
| `first_heartbeat_ok` | serve | gateway can route here |
| `capacity_advertised` | serve | golden + placement quarantine passed; gateway can **place** here ← the real "capacity online" |

`startup-worker.sh` is also the storage admission gate. It leaves Nomad
disabled across reboots, removes stale `/mnt/sandbox-data` fstab rows, mounts
the current `google-sandbox-xfs` device explicitly, and verifies the backing
major:minor, XFS filesystem, and read-write options before and after growing
the filesystem. Any failure stops startup before Nomad can advertise worker
capacity. Run `make validate-infra` to exercise the regression tests.

Because these are absolute timestamps rather than rates, **the normal 10 s scrape
already yields millisecond-accurate boundaries** — no special scrape interval is
needed for a profiling run. Read them straight off a host too:

```bash
curl -sH "Authorization: Bearer $HOST_TOKEN" http://<worker-ip>:8080/metrics | grep boot_phase
```

Grafana: the **Autoscale: worker readiness** row on the Sandbox Fleet dashboard.

Two caveats. `/run` is tmpfs, so the file is per-boot: a **stopped** standby
worker produces a full fresh timeline, while a **resumed suspended** worker keeps
its original boot's phases — correct, because a resumed worker re-runs neither the
startup script nor `serve`, and its readiness path is just resume → network →
next heartbeat. And a host that never warms has no `capacity_advertised`, so
`sandbox_worker_ready_seconds` is absent rather than misleadingly 0.

---

## Static debug VMs

Plain `gcloud` scripts to spin up disposable GCE VMs and tear them down when
you're done. Each VM:

- **8 vCPU / 32 GB RAM** (`n2-standard-8`)
- **512 GB SSD** boot disk (`pd-ssd`)
- Ubuntu 24.04 LTS
- **Spot (preemptible)** — much cheaper, reclaimable by GCP at any time
  (toggle `SPOT` in `config.env`)
- **no service account** attached (`--no-service-account --no-scopes`)
- a **`ayush`** user with **passwordless sudo**
- **Tailscale** installed + joined to your tailnet on first boot (with Tailscale SSH)

Defaults: **2** VMs (`testvm-1`, `testvm-2`).

## Prerequisites

```bash
gcloud auth login
gcloud config set project ratio-experiments
gcloud services enable compute.googleapis.com    # one-time
```

## Usage

```bash
cd infra/gcp
cp config.env.example config.env   # config.env is gitignored — keep secrets here
$EDITOR config.env                 # set PROJECT, and your EPHEMERAL TAILSCALE_AUTHKEY

./vms.sh up                 # create the VMs
./vms.sh list               # status + external/internal IPs
./vms.sh ssh testvm-1       # gcloud ssh into one
./vms.sh down               # delete them all (add -y to skip the prompt)
```

## How it works

- **`config.env`** — all the knobs (project, zone, names, machine type, disk,
  user, Tailscale key). Edit this.
- **`vms.sh`** — `up` / `down` / `list` / `ssh` wrappers around `gcloud`. `up`
  creates every name in `NAMES` in a single `gcloud compute instances create`
  call with `--no-service-account --no-scopes`.
- **`startup.sh`** — runs as root on first boot. Reads `ssh-user`,
  `tailscale-authkey`, and `ssh-pubkey` from instance metadata, then creates the
  user with passwordless sudo and brings up Tailscale. Idempotent. Output is
  logged to `/var/log/startup-script.log` on each VM.

The Tailscale key and any SSH key are passed via instance **metadata**, not
baked into the committed script.

## Connecting

- **Over Tailscale (recommended):** once a box appears in your tailnet,
  `ssh ayush@testvm-1`. Tailscale SSH authorizes you by tailnet identity — no
  keys to manage.
- **Direct:** `./vms.sh ssh testvm-1` (uses `gcloud compute ssh`), or set
  `SSH_PUBLIC_KEY` in `config.env` and `ssh ayush@<external-ip>`.

## Tear down

```bash
./vms.sh down            # or: ./vms.sh down -y
```

Deletes the instances (and their boot disks). The Tailscale auth key is
**ephemeral**, so the nodes auto-remove from your tailnet once they go offline —
no manual cleanup needed.

## Notes

- Provisioning happens on first boot, so the user/Tailscale take ~30–60s after
  the VM shows `RUNNING`. Watch it:
  `./vms.sh ssh testvm-1 -- sudo tail -f /var/log/startup-script.log`
- `config.env` holds your project ID and (if set) the Tailscale key — it's
  gitignored.
- Want different counts/specs? Edit `NAMES`, `MACHINE_TYPE`, `DISK_SIZE`, etc.
  in `config.env`.
