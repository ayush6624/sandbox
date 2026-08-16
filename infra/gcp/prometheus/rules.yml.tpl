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
#
# FLOORED on the provider target size while a gateway scale-out is recent. The
# gateway and this rule are two independent implementations of "how many hosts
# do we need", and they do NOT agree: evaluateDirectScaleOut nudges its answer
# to live+1 whenever the create queue is non-empty, because a queued create
# PROVES the registered fleet can't place it even when the arithmetic says it
# fits. This rule has no such term. So a queue that forms without moving the
# ratio (measured 2026-08-16: queue peaked at 11, gateway grew 2 -> 3, this rule
# stayed at 2) makes the autoscaler read a legitimately-lower number and scale
# straight back in — and with node_purge + newest_create_index it destroys the
# host the gateway just added, killing every sandbox on it (11 x
# `502 host ... unreachable: EOF` mid-sweep).
#
# max_over_time(SCALE_DOWN_WINDOW) in the policy was meant to be that
# protection, but it can only latch values this rule actually emits, and the
# gateway's decision never appeared here. sandbox_scale_out_requested can't be
# the floor either: it re-baselines to the LIVE host count, never below, so
# flooring on it would pin the fleet at its high-water mark forever.
#
# The floor is therefore event-scoped, not level-scoped: while the gateway has
# requested a scale-out in the last 5m, desired is at least the MIG target it
# asked for; after that the floor disappears and the policy's own
# max_over_time window carries the remaining decay. Net effect: no scale-in
# until SCALE_DOWN_WINDOW of quiet has passed since the last scale-out, which
# is the asymmetry this fleet already intends.
#
# The demand arithmetic and the floor are split into their own recording rules
# so the combining expression stays readable — this signal has now silently
# collapsed to a constant TWICE from PromQL label/empty-set semantics, and a
# 1,400-character one-liner is how that keeps happening. Every reference is
# wrapped in sum(): a recording rule's result carries
# __name__="sandbox:...", and two differently-named vectors do NOT match in a
# binary operator, which is precisely the failure mode documented on the
# policy's own query.
groups:
  - name: sandbox
    # Recompute on the 5s gateway scrape cadence. A 10s scrape + 10s rule
    # evaluation left too little of the 30s create-latency SLO for standby
    # resume and sandbox bring-up.
    interval: 5s
    rules:
      # Raw demand arithmetic. Absent when the gateway series are missing, which
      # is what makes the autoscaler HOLD during a gateway/Prometheus outage
      # rather than scale to MIG_MIN — sandbox:workers_desired below preserves
      # that emptiness deliberately.
      - record: sandbox:workers_demand
        expr: clamp_min(ceil((sum(sandbox_slots_committed{job="sandbox-gateway"}) + sum(sandbox_hibernated{job="sandbox-gateway"}) + sum(sandbox_create_queue_depth) + (sum(rate(sandbox_create_rejected_total[1m])) * 5 or vector(0)) + clamp_min((sum(rate(sandbox_creates_total{job="sandbox-gateway"}[2m])) * ${LEAD_SECONDS}) or vector(0), ${HEADROOM_SLOTS})) / ${SLOTS_PER_HOST}), 1)
      # The MIG size the gateway asked for, but only while that request is
      # recent. `or vector(0)` makes it always present, so the max below never
      # collapses to an empty vector when the gateway predates either series.
      - record: sandbox:workers_scale_out_floor
        expr: (sum(sandbox_mig_target_size{job="sandbox-gateway"}) and (sum(increase(sandbox_direct_scale_out_total{job="sandbox-gateway"}[5m])) > 0)) or vector(0)
      # max(demand, floor), and empty when demand is empty. `(A > B) or B` alone
      # would answer B during an outage (B is never empty), turning a dead
      # gateway into a scale-in; `(F and D)` yields the floor's value only where
      # demand exists, and the trailing `or D` covers demand < floor being false
      # because the floor is 0.
      - record: sandbox:workers_desired
        expr: (sum(sandbox:workers_demand) > sum(sandbox:workers_scale_out_floor)) or (sum(sandbox:workers_scale_out_floor) and sum(sandbox:workers_demand)) or sum(sandbox:workers_demand)
      # Ceiling for the autoscaler's SCALE-IN-ONLY policy. The gateway owns
      # scale-out (its direct path reacts in ~1s vs this loop's ~10s, and ~189s
      # if the request lands in the gce-mig confirmation blackout), so the
      # autoscaler must never request MORE nodes than the group is already
      # targeting — two writers that both grow can ratchet it above demand.
      #
      # The ceiling is the PROVIDER's target size, which the gateway polls and
      # exports. It must NOT be derived from heartbeats: sandbox_hosts_live also
      # counts resumed standby workers that sit outside the MIG target, and
      # capping on it let the autoscaler scale out past this cap anyway
      # (measured 2026-07-28: hosts_live=8 vs targetSize=5 admitted a latched
      # max_over_time peak of 6, logged `from=5 to=6 scaling up because metric
      # is 6`). targetSize is the same number the gce-mig target compares
      # against, so min(desired, targetSize) can only ever hold or shrink.
      #
      # Fallback when sandbox_mig_target_size is absent (a gateway predating it,
      # or no successful provider poll yet): max(hosts_live, the gateway's
      # grow-only watermark). That is the previous, looser behaviour — it can
      # still admit a scale-out, but it never blocks scale-in, which is the
      # safer failure direction. `(A > B) or B` is PromQL element-wise max, and
      # `or vector(0)` keeps the expression non-empty so scale-in cannot
      # silently stop.
      - record: sandbox:workers_scale_in_ceiling
        expr: sum(sandbox_mig_target_size{job="sandbox-gateway"}) or ((sum(sandbox_hosts_live{job="sandbox-gateway"}) > (sum(sandbox_scale_out_requested{job="sandbox-gateway"}) or vector(0))) or (sum(sandbox_scale_out_requested{job="sandbox-gateway"}) or vector(0)))

  - name: public-ingress
    interval: 15s
    rules:
      - alert: SandboxEdgeReplicaQuorumLost
        expr: sum(up{job="sandbox-edge"}) < 2
        for: 2m
        labels: { severity: critical }
        annotations:
          summary: Fewer than two public edge replicas are healthy
      - alert: SandboxEdgeCertificateExpiring
        expr: min(sandbox_edge_certificate_expiry_timestamp_seconds{job="sandbox-edge"}) - time() < 7 * 24 * 60 * 60
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: Public ingress certificate expires within seven days
      - alert: SandboxEdgeCertificateReloadFailure
        expr: sum(increase(sandbox_edge_certificate_reloads_total{job="sandbox-edge",result="error"}[15m])) > 0
        labels: { severity: warning }
        annotations:
          summary: An edge replica rejected a certificate update
      - alert: SandboxEdgeResolveFailures
        expr: sum(rate(sandbox_edge_conns_total{job="sandbox-edge",result="error"}[5m])) / clamp_min(sum(rate(sandbox_edge_conns_total{job="sandbox-edge"}[5m])), 0.01) > 0.05
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: More than 5% of edge connections are failing
      - alert: SandboxRawPortPoolNearExhaustion
        expr: sum(sandbox_raw_leases{job="sandbox-gateway",state=~"active|pending|releasing"}) / ${RAW_PORT_CAPACITY} > 0.9
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: Raw public port pool is over 90% allocated
      - alert: SandboxRawLeaseReconciliationStuck
        expr: sum(sandbox_raw_leases{job="sandbox-gateway",state=~"pending|releasing"}) > 0
        for: 10m
        labels: { severity: warning }
        annotations:
          summary: Durable raw port lease reconciliation is stuck
