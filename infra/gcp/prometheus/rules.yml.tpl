# Rendered by control-install.sh (envsubst): ${HEADROOM_SLOTS}, ${SLOTS_PER_HOST},
# ${LEAD_SECONDS}.
# sandbox:workers_desired is the scaling signal: how many worker hosts we want
# so that OCCUPIED capacity PLUS queued creates PLUS a headroom buffer fit.
#
# Occupancy is (slots_committed + hibernated), NOT (slots_total - slots_free).
# slots_committed is each host's running slots plus gateway create reservations,
# clamped to that host's physical capacity. This closes the first-scrape gap
# where a held burst has been assigned but the worker heartbeat has not reported
# the new sandboxes yet; without it, 96 assigned + 64 queued could appear as
# only 48 used + 64 queued and under-scale a four-worker burst to three.
#
# A host
# still WARMING advertises slots_free=0 as a PLACEMENT gate (so it doesn't attract
# a cold-boot storm), yet it runs ZERO sandboxes — so (total - free) misreads it
# as fully occupied, a phantom ~SLOTS_PER_HOST spike per warming host that
# max_over_time then LATCHES into a ~1-host over-scale for the whole scale-down
# window (observed live: a scale-up to 3 bounced to 4). slots_committed is zero
# for a warming host with neither running sandboxes nor reservations; adding
# hibernated keeps the original intent — scale for hibernation-heavy fleets, whose
# frozen sandboxes hold ports and will wake — without the warming artifact.
# (slots_committed + hibernated slightly over-counts hibernation vs total-free, but
# that's conservative and hibernated is ~0 in the steady state.)
#
# Both terms MUST be scoped to {job="sandbox-gateway"} (the gateway's
# fleet-aggregate /metrics): sandbox_hibernated (and the old slots_free) are ALSO
# exported per-host by the federation (job="sandbox-hosts", /metrics/hosts), so an
# unscoped sum() DOUBLE-COUNTS. slots_committed is gateway-only, queue_depth/rejected
# are gateway-only, creates_ok is host-only — but scope the two occupancy terms
# defensively so a future federation of either can't silently corrupt the signal.
#
# Queued creates (the gateway's bounded create queue) are demand that found no
# slot — counting them makes a burst larger than the headroom pull scale-up
# immediately, while the queue holds those creates until the new host boots.
#
# The queue-depth gauge saturates at queue-max, so demand beyond it appears
# ONLY as rejected creates (503 + Retry-After ~5s). Rejected clients that
# retry re-increment the counter every ~5s, so rate()*5 approximates the
# outstanding overflow; `or vector(0)` keeps the rule alive against an old
# gateway that doesn't export the counter yet. sum() strips instance labels
# so the label-less vector(0) can participate in the arithmetic.
# The headroom term LEADS demand instead of being a flat buffer: it reserves
# enough spare slots to absorb the creates expected to arrive during one host's
# reaction window (detection + warm-up), i.e. rate(creates)·LEAD_SECONDS, but
# never drops below the static floor HEADROOM_SLOTS. So an idle fleet keeps the
# fixed floor (rate≈0 → clamp_min(0, HEADROOM) = HEADROOM, unchanged), while a
# sustained ramp pre-provisions ahead of the curve so the create queue rarely
# forms. sandbox_creates_total is the GATEWAY's own aggregate create counter on
# its /metrics (scraped every 10s), so rate() estimates the fleet create rate at
# 10s resolution — far fresher than the 30s-federated per-host
# sandbox_creates_ok_total it replaces (12 vs ~4 samples across the 2m window,
# and ≤10s vs ≤30s stale). rate() handles gateway restarts (counter reset).
# `or vector(0)` keeps the term at the floor if the series is absent (old gateway
# that predates this counter, or no creates yet). Set LEAD_SECONDS=0 to disable
# the lead and fall back to the pure static floor.
#
# Clamped to >=1 (never scale to zero). The autoscaler reads this and (via
# max_over_time in its policy) makes scale-up instant and scale-down slow. If
# the gateway/Prometheus is down the series is absent and the query errors, so
# the autoscaler holds — a safe default.
groups:
  - name: sandbox
    # Recompute on the 5s gateway scrape cadence. A 10s scrape + 10s rule
    # evaluation left too little of the 30s create-latency SLO for standby
    # resume and sandbox bring-up.
    interval: 5s
    rules:
      - record: sandbox:workers_desired
        expr: clamp_min(ceil((sum(sandbox_slots_committed{job="sandbox-gateway"}) + sum(sandbox_hibernated{job="sandbox-gateway"}) + sum(sandbox_create_queue_depth) + (sum(rate(sandbox_create_rejected_total[1m])) * 5 or vector(0)) + clamp_min((sum(rate(sandbox_creates_total{job="sandbox-gateway"}[2m])) * ${LEAD_SECONDS}) or vector(0), ${HEADROOM_SLOTS})) / ${SLOTS_PER_HOST}), 1)
      # Ceiling for the autoscaler's SCALE-IN-ONLY policy. The gateway owns
      # scale-out (its direct path reacts in ~1s vs this loop's ~10s, and ~189s
      # if the request lands in the gce-mig confirmation blackout), so the
      # autoscaler must never request MORE nodes than the fleet already has —
      # two writers that both grow can ratchet the group above demand.
      #
      # max(hosts_live, scale_out_requested), NOT hosts_live alone: for ~13s
      # after the gateway resizes, the new workers exist but have not
      # heartbeated, so hosts_live reads low and an uncapped policy would scale
      # the fleet back down in the middle of the burst it was just sized for.
      # sandbox_scale_out_requested is the gateway's grow-only watermark, which
      # re-baselines to the live host count once the create queue drains, so
      # scale-in still works — just never against an in-flight scale-out.
      #
      # `(A > B) or B` is PromQL element-wise max. `or vector(0)` keeps the rule
      # alive against a gateway that predates sandbox_scale_out_requested;
      # without it the series is absent, the whole expression is empty, and
      # scale-in would silently stop.
      - record: sandbox:workers_scale_in_ceiling
        expr: (sum(sandbox_hosts_live{job="sandbox-gateway"}) > (sum(sandbox_scale_out_requested{job="sandbox-gateway"}) or vector(0))) or (sum(sandbox_scale_out_requested{job="sandbox-gateway"}) or vector(0))
