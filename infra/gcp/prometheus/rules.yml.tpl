# Rendered by control-install.sh (envsubst): ${HEADROOM_SLOTS}, ${SLOTS_PER_HOST}.
# sandbox:workers_desired is the scaling signal: how many worker hosts we want
# so that used slots PLUS a headroom buffer fit. Clamped to >=1 (never scale to
# zero). The autoscaler reads this and (via max_over_time in its policy) makes
# scale-up instant and scale-down slow. If the gateway/Prometheus is down the
# series is absent and the query errors, so the autoscaler holds — a safe
# default.
groups:
  - name: sandbox
    interval: 15s
    rules:
      - record: sandbox:workers_desired
        expr: clamp_min(ceil((sandbox_slots_used + ${HEADROOM_SLOTS}) / ${SLOTS_PER_HOST}), 1)
