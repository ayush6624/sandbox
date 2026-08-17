# GCP infra (gcloud)

Plain `gcloud` scripts for the sandbox fleet on GCP, all in **Mumbai
(`asia-south1`)**. Two paths:

1. **Autoscaling fleet (production)** — a control VM + a Managed Instance Group
   of workers, resized directly by the gateway. See
   **[Autoscaling fleet](#autoscaling-fleet)** below.
2. **Static debug VMs** — hand-named throwaway VMs (`vms.sh` + `fleet-deploy.sh`).
   Good for one-off debugging; documented under
   **[Static debug VMs](#static-debug-vms)**.

---

## Autoscaling fleet

The elastic fleet: `sandbox gateway` places sandboxes, computes memory-aware
demand, and changes the worker MIG target in both directions. Prometheus and
Grafana observe that decision; they do not actuate it. Nomad only schedules
`sandbox serve` as a **system job**, so a newly booted worker starts serving
after joining the cluster.

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
  (:9090) + Prometheus (:9091) + Grafana. Reserved static internal IP;
  Tailscale for laptop access + a **subnet router** advertising the VPC subnet
  (so the laptop can reach sandbox forwarded ports on the VPC-internal workers,
  which are *not* on the tailnet).
- **`sandbox-workers`** — a MIG of `n2-standard-8` Firecracker hosts built from
  the baked `sandbox-worker` image. **Non-spot by default** (running sandboxes
  must not be preempted); set `WORKER_SPOT=true` for a cheap, evictable dev
  fleet. The gateway owns the MIG size between `MIG_MIN` and `MIG_MAX`.

**Bring-up** (from `infra/gcp`, after `cp config.env.example config.env` + edit):

```bash
# one-time: buckets, SAs, image
./snapshot-store.sh init            # snapshot bucket + sandbox-fleet-sa (also used by workers)
./mig.sh init                        # release bucket + grant SA read + firewall check
./bake-image.sh                      # bake the sandbox-worker image (~8-10 min)

# control plane
./control.sh up                      # SA + static IP + create the control VM
#   approve the advertised subnet route in the Tailscale admin console (one-time)
./control.sh deploy                  # gateway + nomad server + Prometheus + Grafana

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
  instead of reinstalling Nomad server/Prometheus/Grafana, which are
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

**Scaling knobs** (`config.env`): `MIG_MIN`/`MIG_MAX` bound cost and fleet size;
`SLOTS_PER_HOST` is the single source of truth for tap/IP capacity;
`MEM_PER_SLOT_MIB` turns each host's memory budget into slot equivalents;
`HEADROOM_SLOTS` keeps placeable capacity ahead of demand;
`SCALE_IN_AFTER_SEC` is the low-demand window before a reversible cordon; and
`QUEUE_WAIT`/`QUEUE_MAX` bound queued creates. The default maximum is 22 × 48 =
1056 ordinary sandboxes, while larger `mem_mib` requests consume proportionally
more demand and admission capacity.

**Scaling ownership:** the gateway is the only production actuator. Its
memory-aware `fleetDemand()` drives target growth; low demand cordons the
emptiest eligible host, waits until it holds no running, hibernated, or
mid-create sandboxes, then deletes that exact MIG instance by name. Prometheus
records the provider target as `sandbox:workers_desired`, and Nomad schedules
one system allocation per live worker, but neither resizes the MIG. The retired
Nomad Autoscaler remains disabled and its configuration is removed.

**Provider standby is forbidden.** GCE's `SCALE_OUT_POOL` policy is another
controller even without a GCE Autoscaler: it replenishes its reserve by
suspending or stopping an arbitrary running group member, with no view of
sandbox occupancy. A live benchmark on 2026-08-17 caught it suspending a worker
that held 40 sandboxes. `STANDBY_SUSPENDED_SIZE` and
`STANDBY_STOPPED_SIZE` must both remain zero; `mig.sh` rejects non-zero values
and enforces `MANUAL` standby mode. Scale-out therefore creates normal MIG
members whose full boot/readiness path is visible and attributable.

**Burst behavior:** a burst first consumes headroom, then waits in the gateway's
bounded queue. Queue reservations and memory-weighted request demand cause the
gateway to raise the MIG target immediately. A fresh worker advertises no free
capacity until its golden snapshot and initial ready pool have settled; there
is no extra boot-age delay now that the provider cannot suspend it. Each worker
bounds concurrent bring-ups, and the capacity heartbeat wakes queued requests.
The expected worker release also gates placement during rollouts. A create that
hits a worker disappearing between selection and dispatch is retried on another
eligible host; only demand beyond `MIG_MAX` or `QUEUE_WAIT` surfaces a capacity
error with `Retry-After`.

**Teardown:** `./mig.sh down` then `./control.sh down` (the reserved IP, SAs, and
buckets persist — remove with `gcloud` if you're fully done).

**Ops:** `./control.sh status`, `./mig.sh status`, and on the control VM
`nomad job status sandbox-serve` / `nomad node status`.

### Profiling a scale-up (worker readiness)

Autoscale latency splits into a **decision** span and a **readiness**
span (resize → the new host advertises capacity), which dominates. The readiness
span is instrumented per stage and exported on every host's `/metrics`, federated
to Prometheus with a `host` label:

Prometheus scrapes gateway state every 10 seconds. The gateway submits both
scale-out and scale-in actions; its direct-scale and drain counters are the
authoritative action history. `sandbox_mig_target_size` is provider intent,
while `sandbox_hosts_live` is the heartbeat-visible fleet.

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

Two caveats. `/run` is tmpfs, so a normal new worker produces a full fresh
timeline; an operator-initiated process restart can omit earlier boot phases.
A host that never warms has no `capacity_advertised`, so
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
