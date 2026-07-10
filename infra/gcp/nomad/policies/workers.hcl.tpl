# Rendered by control-install.sh (envsubst): ${PROJECT} ${ZONE} ${MIG_NAME}
# ${MIG_MIN} ${MIG_MAX} ${SCALE_DOWN_WINDOW}.
#
# Cluster (node-count) scaling policy for the worker MIG. The query is the
# recording rule sandbox:workers_desired, wrapped in max_over_time so:
#   - scale-UP is immediate: a spike makes the max jump this eval.
#   - scale-DOWN is delayed: desired must stay low for the whole window before
#     the max falls — this is our "slow, asymmetric" scale-in without relying on
#     directional cooldown fields (which the policy schema doesn't reliably
#     expose). pass-through feeds the value straight through as the target count.
#
# node_selector_strategy=newest_create_index: Nomad only sees the system job, so
# it can't tell a busy host from an idle one; removing the newest instance is
# statistically the emptiest under bin-pack placement. Scale-in kills running
# sandboxes on the chosen host by design (saved snapshots survive via GCS).
scaling "sandbox_workers" {
  # type=cluster scales NODES via a cluster target (gce-mig). Without it the
  # policy defaults to type=horizontal (scale a Nomad job's task group) and the
  # autoscaler silently never evaluates it against the MIG target.
  type    = "cluster"
  enabled = true
  min     = ${MIG_MIN}
  max     = ${MIG_MAX}

  policy {
    cooldown            = "2m"
    evaluation_interval = "30s"

    check "workers_desired" {
      source = "prometheus"
      query  = "max_over_time(sandbox:workers_desired[${SCALE_DOWN_WINDOW}])"

      strategy "pass-through" {}
    }

    target "gce-mig" {
      project                = "${PROJECT}"
      zone                   = "${ZONE}"
      mig_name               = "${MIG_NAME}"
      node_class             = "sandbox-worker"
      node_drain_deadline    = "2m"
      node_purge             = "true"
      node_selector_strategy = "newest_create_index"
    }
  }
}
