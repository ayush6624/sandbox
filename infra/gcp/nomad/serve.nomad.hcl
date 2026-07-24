# The `sandbox serve` host agent, delivered to every worker node as a Nomad
# SYSTEM job. Submitted by deploy-job.sh with vars from fleet-secrets.env +
# config.env. A node that joins with node.class=sandbox-worker runs this within
# seconds — that IS the autoscale "new capacity comes online" path.
#
# raw_exec (not exec): serve needs root, host networking (creates br-fc, taps,
# iptables DNAT), /dev/kvm, and the shared XFS mount — the exec driver's
# namespace/chroot isolation fights all of it. The Nomad client runs as root,
# so raw_exec tasks do too.

variable "gateway_url"   { type = string }
variable "gateway_token" { type = string }
variable "host_token"    { type = string }
variable "release"       { type = string }              # git sha under releases/
variable "bucket"        { type = string }              # GCS release bucket
variable "config_path"   { type = string }              # path to devbox-gcp.json on the submitting host
variable "task_cpu"      { type = number }              # cgroup CPU shares; deploy-job.sh sizes from the machine type
variable "task_memory"   { type = number }              # cgroup memory.max MiB; deploy-job.sh sizes from SLOTS_PER_HOST

job "sandbox-serve" {
  datacenters = ["dc1"]
  type        = "system"

  constraint {
    attribute = "${node.class}"
    value     = "sandbox-worker"
  }

  group "serve" {
    # System jobs have no reschedule policy (a per-host agent isn't movable).
    # restart handles in-place recovery if serve exits.
    restart {
      attempts = 3
      interval = "5m"
      delay    = "10s"
      mode     = "delay"
    }

    task "serve" {
      driver = "raw_exec"

      artifact {
        source      = "gcs::https://www.googleapis.com/storage/v1/${var.bucket}/releases/${var.release}/sandbox"
        destination = "local/bin/sandbox"
        mode        = "file"
      }
      artifact {
        source      = "gcs::https://www.googleapis.com/storage/v1/${var.bucket}/releases/${var.release}/sandboxd"
        destination = "local/bin/sandboxd"
        mode        = "file"
      }

      template {
        # Read at job-submit time on the control VM (deploy-job.sh copies the
        # config there first), baked into the job as this template.
        data        = file(var.config_path)
        destination = "local/config.json"
      }

      template {
        destination = "local/run.sh"
        perms       = "755"
        data        = <<EOT
#!/bin/bash
set -euo pipefail
cd "$${NOMAD_TASK_DIR}"
chmod +x bin/sandbox bin/sandboxd
# Boot-phase stamp (internal/server/bootphase.go). Nomad fetches the `artifact`
# blocks BEFORE running this script, so reaching here means "alloc placed +
# release binaries downloaded" — the gap from nomad_started to here is the
# Nomad scheduling + GCS artifact-pull cost on the scale-out path. serve reads
# this file moments later and exports the whole timeline on /metrics.
mkdir -p /run/sandbox 2>/dev/null || true
printf '%s\t%s\n' serve_task_started "$(date +%s%3N)" >> /run/sandbox/boot-phases 2>/dev/null || true
# sandboxd is baked into the base rootfs at IMAGE BUILD time (bake-image.sh
# [3b/6]); the golden snapshot is built from that rootfs and its validity is
# keyed on the base rootfs mtime+size (goldenUsable). So the base rootfs MUST
# stay byte-for-byte immutable at boot. We deliberately do NOT run install-agent
# here: it re-bakes whenever the GCS release sandboxd differs from the baked one
# — and two independent build paths differ by build-stamp bytes alone — which
# bumps the mtime and forces every host to cold-build the golden instead of
# adopting it, defeating the whole point of the baked golden data disk. sandboxd
# is image-pinned: to ship a new agent, rebake (./bake-image.sh bake && golden)
# and roll. (The pulled sandboxd artifact is left in place, unused, for now.)
exec ./bin/sandbox serve --config config.json \
  --listen  "$${NODE_IP}:8080" \
  --advertise "$${NODE_IP}:8080" \
  --token "$${HOST_TOKEN}" \
  --gateway "$${GATEWAY_URL}" --gateway-token "$${GATEWAY_TOKEN}"
EOT
      }

      config {
        command = "/bin/bash"
        args    = ["local/run.sh"]
      }

      env {
        # Nomad interpolates node attributes here; the VPC-internal IP is the
        # client's fingerprinted primary address.
        NODE_IP       = "${attr.unique.network.ip-address}"
        HOST_TOKEN    = "${var.host_token}"
        GATEWAY_URL   = "${var.gateway_url}"
        GATEWAY_TOKEN = "${var.gateway_token}"
      }

      kill_signal  = "SIGTERM"   # serve tears down its VMs gracefully on SIGTERM
      kill_timeout = "120s"      # allow time to destroy up to a full host of VMs

      # serve OWNS the whole host: it launches every firecracker guest as a
      # child process, so the task's cgroup must fit all of them. Nomad (cgroups
      # v2) sets memory.max from `memory`; too low a value OOM-kills the guests
      # (a 512 MiB cap kills every 1 GiB microVM). deploy-job.sh derives both
      # values from config.env — memory from SLOTS_PER_HOST (~1.18 GiB/slot +
      # serve overhead), CPU shares near the machine's core count so guests
      # aren't throttled under contention (shares-based, not a hard cap).
      resources {
        cpu    = var.task_cpu
        memory = var.task_memory
      }
    }
  }
}
