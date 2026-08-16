# Rendered by control-install.sh (envsubst): ${PROJECT} ${ZONE} ${MIG_NAME}
# ${MIG_MIN} ${MIG_MAX} ${SCALE_DOWN_WINDOW} ${AUTOSCALER_RETRY_ATTEMPTS}.
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
    # Detection lag = Prometheus scrape (5s) + rule eval (5s) + this interval,
    # so 5s here caps worst-case signal-to-action at ~15s. The previous 10s
    # stages produced a 16s observed decision and pushed held-burst p95 create
    # latency above the 30s SLO even after demand accounting was corrected.
    # cooldown gates only how soon the NEXT action can fire — 1m lets successive burst-waves scale up
    # promptly; scale-DOWN slowness comes from max_over_time(SCALE_DOWN_WINDOW)
    # in the check below, NOT from cooldown, so a short cooldown can't cause
    # scale-in flapping. With lever-1 baked-golden adoption a new host warms in
    # ~60-90s, so shaving ~25s off detection is a meaningful fraction.
    cooldown            = "1m"
    evaluation_interval = "5s"

    # INTENDED TO BE SCALE-IN ONLY, BUT NOT YET AIRTIGHT. Measured 2026-07-28:
    # this policy still scaled out (from=5 to=6) after a burst drained, because
    # the ceiling below uses sandbox_hosts_live, which counts resumed standby
    # workers that heartbeat to the gateway without being part of the MIG
    # targetSize the autoscaler compares against — so hosts_live=8 vs target=5
    # left room for a latched max_over_time peak of 6. Fix is to cap on the
    # MIG's real targetSize (the gateway holds the GCE client and permission to
    # read and export it). See docs/autoscaling-latency.md "Known gap".
    #
    # The gateway is the sole scale-OUT writer (its
    # queue-triggered, level-triggered direct path reacts in ~1s where this loop
    # needs ~10s, and ~189s if the request lands in the confirmation blackout).
    # Two independent writers that GROW the group can ratchet it above demand,
    # so this policy is capped to never request more than the fleet already has.
    #
    # The cap is max(hosts_live, scale_out_requested), not hosts_live alone:
    # for ~13s after the gateway resizes, the new workers exist but have not
    # heartbeated, so hosts_live reads low and an uncapped policy would scale
    # the fleet straight back down in the middle of the burst it was sized for.
    # scale_out_requested is the gateway's grow-only watermark, which
    # re-baselines to the live host count once the create queue drains — so
    # scale-in still works, just never against an in-flight scale-out.
    #
    # `(A < B) or B` is PromQL's element-wise min: the comparison yields A when
    # A<B and nothing otherwise, so `or B` supplies B in the other case. Both
    # sides reduce to an empty label set here, so they match. The ceiling itself
    # is the recording rule sandbox:workers_scale_in_ceiling. Verified against
    # the live fleet in both directions before deploying.
    # The outer sum() is REQUIRED, not cosmetic. `(A < B) or B` returns A when
    # the comparison holds and B otherwise, and those two branches have
    # DIFFERENT label sets: max_over_time() strips __name__, so branch A is
    # label-free, while branch B is the raw recording rule and still carries
    # __name__="sandbox:workers_scale_in_ceiling". The autoscaler's Prometheus
    # APM plugin reads a named result as no data, so the metric silently became
    # 0 and pass-through produced a target of 0 (capped to min). Measured
    # 2026-07-28: 259 consecutive evaluations logged count.original:0 with an
    # empty reason_history, and the fleet could not scale in at all. It fails
    # exactly when the ceiling BINDS — i.e. during a burst — which is the worst
    # possible time. sum() drops every label, so both branches present the same
    # single label-free sample the plugin got before this cap existed.
    #
    # If you are here because scale-in fired against a host the GATEWAY had
    # just added, the fix is not in this file: sandbox:workers_desired is
    # floored on sandbox:workers_scale_out_floor while a gateway scale-out is
    # recent (prometheus/rules.yml.tpl). The value read here was never wrong —
    # the gateway sizes to live+1 on a non-empty create queue and this signal
    # has no such term, so the two writers disagreed by exactly one host and
    # the autoscaler's answer, applied through node_purge, deleted live
    # sandboxes (2026-08-16). Do not diagnose that as an APM misread.
    check "workers_desired" {
      source = "prometheus"
      query  = "sum((max_over_time(sandbox:workers_desired[${SCALE_DOWN_WINDOW}]) < sandbox:workers_scale_in_ceiling) or sandbox:workers_scale_in_ceiling)"

      strategy "pass-through" {}
    }

    target "gce-mig" {
      project                = "${PROJECT}"
      zone                   = "${ZONE}"
      mig_name               = "${MIG_NAME}"
      # Confirmation budget = scale-up blackout: the policy stays in
      # StateScaling while the target polls for MIG-wide stability, dropping
      # every evaluation in that window ("skipping scaling, target still
      # scaling"). The 15-attempt default (150s) is why scale-outs logged
      # "failed to confirm scale out ... reached retry limit" ~2m24s in — and
      # with a standby pool it can never confirm, since background replenish
      # keeps the MIG unstable ~190s. Fail fast instead: readiness is observed
      # from the gateway heartbeat, and a failed confirm returns to Idle with no
      # cooldown, so a second burst wave can scale within ~30s instead of ~150s.
      # Requires autoscaler >= 0.4.8; older builds ignore this key.
      retry_attempts         = "${AUTOSCALER_RETRY_ATTEMPTS}"
      node_class             = "sandbox-worker"
      node_drain_deadline    = "2m"
      node_purge             = "true"
      node_selector_strategy = "newest_create_index"
    }
  }
}
