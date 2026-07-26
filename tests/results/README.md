# E2e run results

Output of the fleet e2e suite (`npm run e2e`, see [`../README.md`](../README.md)).
One `run_<ISO timestamp>.json` per invocation; everything here except this README
is **gitignored**.

Live autoscaling runs use a directory named `autoscale-<UTC>/`:

- `traffic.json` or `burst.json`: driver measurements and acceptance result;
- `timeline.jsonl`: MIG, Nomad, gateway, queue, and demand events on one clock;
- `benchmark.log`: complete driver output;
- `mig-latest.json`: latest compact physical-fleet observation;
- `run.json`: immutable run identity, commit/release, bounds, exit code,
  cleanup verdict, and final sandbox/host/MIG state;
- `SHA256SUMS`: checksums for the captured files.

Archive the whole directory. A driver result without `run.json` and a
`cleanup_ok: true` verdict is incomplete evidence, even when its measurements
look successful.

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
