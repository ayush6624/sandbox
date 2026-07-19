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

**Drive it** from the laptop over Tailscale (token printed by `control.sh status`):

```bash
SANDBOX_API_URL=http://<control-tailnet-ip>:9090 SANDBOX_API_KEY=<gateway-token> \
  tsx ../../sdk/typescript/benchmarks/fleet-bench.ts --count 20
```

**Iterate on the Go binaries** without re-baking the image:

```bash
make -C ../.. gcs-release && ./deploy-job.sh   # new sha rolls the system job fleet-wide
```

That rolls the WORKERS only. If the change touches the gateway — including
adding fields to `client.CreateOpts` (the gateway re-encodes create bodies
through it, so an old gateway silently drops new fields) — also run
`./control.sh deploy` to update the control plane.

**Scaling knobs** (`config.env`): `MIG_MIN`/`MIG_MAX` (bounds + cost guardrail),
`SLOTS_PER_HOST` (the **single source of truth** for per-host capacity —
`deploy-job.sh` *generates* the pools in `devbox-gcp.json` from it: taps = IPs
= N, ports = 4N so hibernated port-holds and extra exposed ports never bind
capacity, plus `mem_budget_mib = N×1180` so `mem_mib` overrides are admitted
against the host's real memory — a big-mem sandbox consumes multiple slots'
worth of `slots_free` and can never OOM the cgroup; max 200 per the /24 guest
subnet), `HEADROOM_SLOTS` (free slots kept
ahead of demand), `SCALE_DOWN_WINDOW` (how long demand must stay low before
scale-in), `STANDBY_STOPPED_SIZE` (pre-created stopped VMs the MIG starts on
scale-up — tens of seconds to serving instead of the minutes a fresh
create+boot takes; apply to a live MIG with `./mig.sh standby`), and
`QUEUE_WAIT`/`QUEUE_MAX` (the gateway's create queue — wait must cover standby
start → nomad join → golden-snapshot build, ~2-3 min). The defaults size the
fleet for **1000 concurrent sandboxes**: n2-standard-16 workers × 48 slots ×
MIG_MAX=22. Scale-up is immediate; scale-down waits out the window.
**Scale-in kills running sandboxes on the removed host** (saved snapshots
survive via GCS durability); bin-pack placement + the window minimize how
often that hits an in-use host.

**Burst behavior** end to end: a burst first lands on `HEADROOM_SLOTS` of free
capacity; overflow creates wait in the gateway's bounded queue
(`QUEUE_WAIT`/`QUEUE_MAX`) instead of 503ing, and the queue depth itself feeds
`sandbox:workers_desired` — computed from **effective occupancy**
(`slots_total − slots_free`, so capacity held by hibernated sandboxes' ports
or still-warming hosts counts as demand) — so the autoscaler scales up
immediately; the MIG serves that resize from the stopped standby pool in
~30-60 s (falling back to fresh creates when the pool runs dry). A fresh host
advertises `slots_free=0` until its golden snapshot is built, so it is never
boot-stormed with cold creates; each host also bounds concurrent bring-ups
(`create_concurrency`, default 2×cores capped at 16). A create that still hits
a stale host gets failed over to the next host by the gateway (up to 3
attempts) before it would ever surface an error. Only a burst that outruns
queue-wait + MIG_MAX sees 503s (with Retry-After).

**Teardown:** `./mig.sh down` then `./control.sh down` (the reserved IP, SAs, and
buckets persist — remove with `gcloud` if you're fully done).

**Ops:** `./control.sh status`, `./mig.sh status`, and on the control VM
`nomad job status sandbox-serve` / `nomad node status`.

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
