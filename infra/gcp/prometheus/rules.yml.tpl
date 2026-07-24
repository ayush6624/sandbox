# Rendered by control-install.sh (envsubst): ${HEADROOM_SLOTS}, ${SLOTS_PER_HOST},
# ${LEAD_SECONDS}.
# sandbox:workers_desired is the scaling signal: how many worker hosts we want
# so that OCCUPIED capacity PLUS queued creates PLUS a headroom buffer fit.
#
# Occupancy is (slots_used + hibernated), NOT (slots_total - slots_free). A host
# still WARMING advertises slots_free=0 as a PLACEMENT gate (so it doesn't attract
# a cold-boot storm), yet it runs ZERO sandboxes — so (total - free) misreads it
# as fully occupied, a phantom ~SLOTS_PER_HOST spike per warming host that
# max_over_time then LATCHES into a ~1-host over-scale for the whole scale-down
# window (observed live: a scale-up to 3 bounced to 4). slots_used is the
# host-reported RUNNING count (a warming host contributes 0, no phantom); adding
# hibernated keeps the original intent — scale for hibernation-heavy fleets, whose
# frozen sandboxes hold ports and will wake — without the warming artifact.
# (slots_used + hibernated slightly over-counts hibernation vs total-free, but
# that's conservative and hibernated is ~0 in the steady state.)
#
# Both terms MUST be scoped to {job="sandbox-gateway"} (the gateway's
# fleet-aggregate /metrics): sandbox_hibernated (and the old slots_free) are ALSO
# exported per-host by the federation (job="sandbox-hosts", /metrics/hosts), so an
# unscoped sum() DOUBLE-COUNTS. slots_used is gateway-only, queue_depth/rejected
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
# forms. creates_ok is a per-host counter federated with a host label; sum(rate)
# is the fleet create rate (rate() handles hosts appearing/disappearing and
# serve-restart counter resets). `or vector(0)` keeps the term at the floor if
# the series is absent (old gateway / no creates yet). Set LEAD_SECONDS=0 to
# disable the lead and fall back to the pure static floor.
#
# Clamped to >=1 (never scale to zero). The autoscaler reads this and (via
# max_over_time in its policy) makes scale-up instant and scale-down slow. If
# the gateway/Prometheus is down the series is absent and the query errors, so
# the autoscaler holds — a safe default.
groups:
  - name: sandbox
    # 10s (was 15s): recompute the scale-up signal on the scrape cadence so the
    # autoscaler (also 10s now) never acts on a stale desired-count.
    interval: 10s
    rules:
      - record: sandbox:workers_desired
        expr: clamp_min(ceil((sum(sandbox_slots_used{job="sandbox-gateway"}) + sum(sandbox_hibernated{job="sandbox-gateway"}) + sum(sandbox_create_queue_depth) + (sum(rate(sandbox_create_rejected_total[1m])) * 5 or vector(0)) + clamp_min((sum(rate(sandbox_creates_ok_total[2m])) * ${LEAD_SECONDS}) or vector(0), ${HEADROOM_SLOTS})) / ${SLOTS_PER_HOST}), 1)
