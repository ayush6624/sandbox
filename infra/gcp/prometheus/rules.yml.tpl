# Rendered by control-install.sh (envsubst): ${HEADROOM_SLOTS}, ${SLOTS_PER_HOST}.
# sandbox:workers_desired is the scaling signal: how many worker hosts we want
# so that OCCUPIED capacity PLUS queued creates PLUS a headroom buffer fit.
#
# Occupancy is (slots_total - slots_free), not slots_used: slots_free is the
# hosts' self-reported allocatable capacity, which subtracts port holds by
# hibernated sandboxes and still-warming hosts — capacity that is consumed
# without being "used". Counting it makes hibernation-heavy fleets scale up
# instead of silently shrinking.
#
# Queued creates (the gateway's bounded create queue) are demand that found no
# slot — counting them makes a burst larger than the headroom pull scale-up
# immediately, while the queue holds those creates until the new host boots.
# Clamped to >=1 (never scale to zero). The autoscaler reads this and (via
# max_over_time in its policy) makes scale-up instant and scale-down slow. If
# the gateway/Prometheus is down the series is absent and the query errors, so
# the autoscaler holds — a safe default.
groups:
  - name: sandbox
    interval: 15s
    rules:
      - record: sandbox:workers_desired
        expr: clamp_min(ceil(((sandbox_slots_total - sandbox_slots_free) + sandbox_create_queue_depth + ${HEADROOM_SLOTS}) / ${SLOTS_PER_HOST}), 1)
