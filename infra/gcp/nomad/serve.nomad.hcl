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

job "sandbox-serve" {
  datacenters = ["dc1"]
  type        = "system"

  constraint {
    attribute = "${node.class}"
    value     = "sandbox-worker"
  }

  group "serve" {
    # A worker either runs serve or it doesn't; no point rescheduling a
    # per-host agent elsewhere.
    reschedule { attempts = 0 unlimited = false }
    restart { attempts = 3 interval = "5m" delay = "10s" mode = "delay" }

    task "serve" {
      driver = "raw_exec"

      artifact {
        source      = "gcs::https://www.googleapis.com/storage/v1/${var.bucket}/releases/${var.release}/sandbox"
        destination = "local/bin"
        mode        = "file"
      }
      artifact {
        source      = "gcs::https://www.googleapis.com/storage/v1/${var.bucket}/releases/${var.release}/sandboxd"
        destination = "local/bin"
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
# Bake the freshly pulled sandboxd into the staged base rootfs (idempotent).
./bin/sandbox install-agent --config config.json --agent ./bin/sandboxd
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

      resources {
        cpu    = 1000   # advisory only under raw_exec
        memory = 512
      }
    }
  }
}
