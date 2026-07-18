# E2e run results

Output of the fleet e2e suite (`npm run e2e`, see [`../README.md`](../README.md)).
One `run_<ISO timestamp>.json` per invocation; everything here except this README
is **gitignored**.

## Shape

```json
{
  "target": "http://<gateway>:9090",
  "startedAt": "...",
  "totalMs": 213420,
  "summary": { "passed": 10, "failed": 5, "skipped": 0 },
  "results": [
    { "suite": "hibernate", "name": "...", "ok": true, "ms": 5185.7 },
    { "suite": "ports", "name": "...", "ok": false, "ms": 18849.2, "error": "AssertionError: ..." }
  ]
}
```

`ms` is wall time per test, so these files double as coarse latency records —
e.g. the `hibernate` suite's wake-and-exec test completing in ~5 s includes a
full freeze + wake + exec round trip.

## Reading a failed run

Mass timeouts across a whole suite usually mean the **network path is down, not
the product**: the `ports` suite dials worker VMs directly (`10.160.x.x`), which
requires the laptop→worker Tailscale subnet route to be approved. If every
`ports` test times out while `hibernate`/`clock` pass, re-run from the control
VM (`sandbox-control`) before diagnosing anything else.
