# Snapshot fan-out — benchmark results

Measured on the GCP test fleet (2× `n2-standard-8`, 8 vCPU / 32 GB, Spot),
guests 2 vCPU / 1 GB, Firecracker v1.15.0, rootfs/snapshots on XFS reflink
storage. Single-host runs on testvm-1; throughput via the gateway across both
hosts. "Latency" = time until the in-guest agent answers (time to a usable box).

Interactive report: [`snapshot-fanout-bench-report.html`](./snapshot-fanout-bench-report.html).
Raw JSON: [`../sdk/typescript/benchmarks/results/extensive/`](../sdk/typescript/benchmarks/results/extensive/).

## Headline

| Metric | Result |
|---|---|
| Restore vs cold boot (p50) | **16.3×** — 212 ms vs 3463 ms |
| Fan-out, 32 clones | **2.68 s** total, **83 ms/clone** amortized |
| Fan-out vs cold boot @ 64 | **12.7×** — 5.7 s vs 72.8 s |
| Memory @ 64 (PSS) | **10.9× less** — 925 MB vs 10.1 GB |

## 1 · Latency (single host, 25 iters)

| Method | mean | p50 | p90 | min | max |
|---|--:|--:|--:|--:|--:|
| Cold boot | 3467 | 3463 | 3507 | 3245 | 3696 |
| Restore | 212 | 212 | 219 | 208 | 227 |

Restore skips kernel boot + init + agent startup (the agent is already running
in restored memory). Snapshot capture ~1.99 s, paid once.

## 2 · Fan-out scaling (single host)

| N | batch (ms) | per-clone (ms) | usable |
|--:|--:|--:|--:|
| 1 | 1695 | 1695.0 | 1/1 |
| 2 | 1697 | 848.5 | 2/2 |
| 4 | 1715 | 428.8 | 4/4 |
| 8 | 1797 | 224.6 | 8/8 |
| 16 | 1994 | 124.6 | 16/16 |
| 32 | 2675 | 83.6 | 32/32 |
| 48 | 3435 | 71.6 | 48/48 |
| 64 | 5719 | 89.4 | 64/64 |
| 64 (cold-boot baseline) | 72797 | 1137.5 | — |

Amortized per-clone cost collapses with N because a fixed ~1.5 s reidentify wait
is shared across the batch. Best per-clone ~72 ms at N=48; it rises at N=64 as
the 64/host pool ceiling is approached. **Open optimization:** replace the fixed
1.5 s wait with a vsock/ARP readiness signal (would cut N=1 to ~200 ms).

## 3 · Fleet throughput (via gateway, both hosts)

| Config | created | workload ok | wall (s) | create p50 (s) | workload p50 (s) |
|---|--:|--:|--:|--:|--:|
| 32 · default | 32 | 32 | 62.8 | 8.64 | 32.94 |
| 64 · default | 64 | 64 | 120.2 | 9.15 | 62.31 |
| 64 · fsync | 64 | 64 | 89.7 | 9.09 | 31.30 |
| 64 · large | 64 | 64 | 318.2 | 9.06 | 250.90 |
| 128 · default | 127 | 57 | 278.6 | 9.33 | 145.07 |

Create latency stays flat (~9 s p50) across sizes — provisioning scales cleanly.
The 128 result is a **compute** ceiling, not a provisioning failure: all were
created, but 128 × 2 vCPU on two 8-core hosts is 16:1 oversubscription, so the
CPU-bound workload saturates the hosts and execs drop.

## 4 · Memory density (single host, N=64)

| via | procs | Σ RSS (MB) | Σ PSS (MB) | PSS/clone (MB) |
|---|--:|--:|--:|--:|
| Fan-out | 64 | 5439 | 925 | 14.5 |
| Cold boot | 64 | 10205 | 10055 | 157.1 |

Fan-out clones `mmap` the same snapshot memory file, so pages are shared in the
page cache (copy-on-write until written). PSS counts shared pages once → the true
footprint is ~925 MB (~72 MB shared + ~13 MB private/clone) vs 10 GB for cold
boots that share nothing.

## Reproduce

```bash
# fleet up + bootstrapped (see infra/gcp + memory gcp-sandbox-fleet)
HOST_IP=<testvm-1 tailnet ip> SSH_HOST=<testvm-1 tailnet ip> \
  HOST_TOKEN=... GATEWAY_TOKEN=...  bash scripts/bench-extensive.sh
```
Tokens are in `infra/gcp/fleet-secrets.env`. Individual benches:
`npm run bench:restore | bench:fanout` and `benchmarks/fleet-bench.ts`.
