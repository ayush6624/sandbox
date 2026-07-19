# Benchmarks

**Updated 2026-07-18** (numbers measured 2026-07-01 → 2026-07-12, server release
`b801d6d`+). Interactive version: [`benchmark-report.html`](./benchmark-report.html)
(published at <https://claude.ai/code/artifact/f14de3c5-96c3-45d1-bc7d-1a4ce4ccf6b3>).

## Headline

| Metric | Result | Clock |
|---|---|---|
| Hot create (golden-snapshot clone) | **199–271 ms** server-side, ~0.55 s end-to-end via gateway | create request → in-guest agent answers |
| Hibernation wake, same identity | **49 ms** server-side | wake start → agent answers |
| Wake-on-connect (forwarded port) | **133 ms** | TCP connect to host port → guest responds, incl. wake |
| Snapshot restore (1:1) | **212 ms p50** (pure resume ~14 ms; rest is rootfs reflink) | restore request → agent answers |
| Fan-out | **83 ms/clone** amortized (32 clones in 2.68 s), 64/64 usable at N=64 | fanout request → all agents answer |
| Diff snapshot write | **123 ms** (vs ~1.5 s full); uploads ~24× smaller | pause → snapshot written |
| Cold boot (baseline) | 3.46 s p50 (GCP), ~2.2 s (Hetzner bare metal) | create request → agent answers |
| Burst churn | 499/500 creates on 3 hosts (72 slots), 6.9 creates/s sustained | 500 create→exec→kill @ concurrency 96 |

Environment: GCP `n2-standard-8` hosts (8 vCPU / 32 GB, nested KVM), guests
2 vCPU / 1 GB, Firecracker v1.15.0, XFS reflink storage; client on the same
tailnet. "Usable" always means the in-guest agent answers — never "the create
call returned".

## Our numbers in detail

### Create

| Path | Latency | Notes |
|---|--:|---|
| Hot create (default) | 199–271 ms server-side | golden-snapshot clone + GARP readiness; 8 concurrent in 1.1 s |
| Hot create, end-to-end | ~0.55 s | via gateway, client on tailnet |
| Hot create, diff-snapshots enabled | 454 ms | measured 2026-07-06, unregressed vs full-snapshot golden |
| Cold boot | 3.46 s p50 | also the fallback path if the golden snapshot is missing |
| Cold boot with `vcpus`/`mem_mib` override | ~3.7 s | overrides always cold-boot (resources are baked into snapshots) |

### Snapshot restore & fan-out

Restore p50 **212 ms** (mean 212, p90 219, 25 iters) vs cold boot p50 3463 ms —
**16.3×**. The actual Firecracker resume is ~14 ms (load+resume 12 ms, agent
2 ms — the agent is already running in restored, lazily-faulted memory); the
rest is the rootfs reflink copy. Cross-host restore from GCS (owner host dead):
**~180 ms** once the base image is cached (rootfs cp 8 ms + load 35 ms + agent
139 ms); first pull of a 2.1 GiB base costs a one-time 13.2 s per host.

Fan-out scaling (single host, measured 2026-07-01, pre-GARP for N>1 batches —
single-clone latency now matches hot create):

| N | batch (ms) | per-clone (ms) | usable |
|--:|--:|--:|--:|
| 1 | 1695 | 1695 | 1/1 |
| 8 | 1797 | 225 | 8/8 |
| 16 | 1994 | 125 | 16/16 |
| 32 | 2675 | 84 | 32/32 |
| 48 | 3435 | 72 | 48/48 |
| 64 | 5719 | 89 | 64/64 |

64 clones in 5.7 s vs 72.8 s of cold boots (**12.7×**); memory density at N=64
is **925 MB PSS vs 10.1 GB** (10.9×) because clones mmap the same snapshot
memory file. Runnable suite: [`sdk/typescript/benchmarks/`](../sdk/typescript/benchmarks/README.md).

### Hibernation

Idle sandboxes are paused + full-snapshotted (~2.5 s), the VM killed, tap/IP
returned to the pools. Wake is transparent on any API call or forwarded-port
connection:

| Path | Latency |
|---|--:|
| Wake on API request, same identity (common case) | **49 ms** server-side |
| Wake on TCP connect to a forwarded port | **133 ms** incl. guest dial |

Frozen background processes survive; the guest wall clock is re-stepped
deterministically on every resume path (`/clock` push + MMDS epoch).

### Burst & fleet

500-sandbox churn (create → exec → kill, concurrency 96) against 3 workers
× 24 slots: **499/500 succeeded** in 72 s (6.9 creates/s) with client
retry+backoff; 0 pool-exhaustion errors after gateway reserve-at-pick, clean
503s under overload. Sustained overload (500 held alive) ramped the autoscaler
3→8 hosts over ~450 s and cleanly rejected the rest. Full write-up:
<https://claude.ai/code/artifact/0cfd2df8-177f-4793-b415-0f4260b51b8b>.

## Versus hosted sandbox providers

Comparing sandbox latencies across providers is mostly an exercise in noticing
that nobody's clock measures the same thing. Vendor "start" numbers variously
mean server-side VM resume, API-to-ready, or "the create call returned" (which
can precede the guest being usable). The most comparable independent dataset is
the [ComputeSDK sandbox leaderboard](https://www.computesdk.com/benchmarks/sandboxes/)
(run 2026-07-17, 100 iterations/provider), which measures **TTI =
`create()` → first successful command**. Ours is self-hosted, so our closest
equivalent is end-to-end create via the gateway from a tailnet client — no WAN
hop to a provider API, which is a real advantage of self-hosting but also means
the numbers aren't apples-to-apples. Both are shown.

### Create → command running

| Provider | Measured (median TTI) | Vendor claim | Claim caveat |
|---|--:|--:|---|
| **This project (self-hosted)** | **~0.55 s** end-to-end · 0.25 s server-side | — | our fleet, tailnet client |
| Vercel Sandbox | 0.40 s (p95 0.59) | none published | fastest on the July 2026 board |
| e2b | 0.48 s (p95 0.81) | "80 ms" / "<200 ms" | same-region, default template only |
| Daytona | 0.58 s (p95 1.12) | "sub-90 ms" | warm pre-pulled images, best case |
| Modal | 0.62 s (p95 0.77) | <0.5 s median | honest self-definition: client → code running |
| Cloudflare Sandboxes | 4.26 s (p95 5.10) | "milliseconds" | claim is the V8-isolate layer; containers are 1–3 s by their own docs |
| CodeSandbox / Together | 6.37 s (p95 8.60) | 2.7 s p95 cold | leaderboard likely hits the non-hibernated path |
| Fly Machines | — | start ~300 ms · create ~5–20 s | start = existing stopped machine, server-side |
| Firecracker (raw VMM) | — | ~125 ms | kernel→init only; the floor everyone builds on, not a product |

### Resume / wake from pause

| Provider | Number | Status |
|---|--:|---|
| **This project** | **49 ms** same-identity · **133 ms** wake-on-connect | measured on our fleet |
| Morph Cloud | <250 ms | vendor claim, combined "snapshot, branch, restore" figure |
| Fly Machines | "a few hundred ms" | official docs; ≤2 GB RAM recommended, snapshot not guaranteed to persist |
| CodeSandbox / Together | 500 ms p95 | vendor claim; 2024 eng blog says "within a second" |
| Vercel Sandbox | p75 <1 s, p95 <10 s | published percentiles; ~6 s penalty on snapshot cache miss |
| e2b | ~1 s | official docs; pause costs 4 s per GiB of RAM |
| Modal (Sandboxes) | not published | Functions memory-snapshot restore: 0.69–1.05 s |
| Daytona, Cloudflare | not published | |

### Fork / clone a running or snapshotted VM

| Provider | Number | Status |
|---|--:|---|
| **This project** | **84 ms/clone** amortized (32 in 2.68 s); single clone ≈ hot create ~250 ms | measured |
| Morph Cloud | <250 ms; "dozens of instances in milliseconds" | vendor claim / press |
| CodeSandbox / Together | ~0.5 s fork overhead (docs); 2 s in the 2022 eng blog | vendor docs + eng blog |
| Modal | supported (N restores from one snapshot) | no latency published |
| e2b, Daytona, Vercel, Fly, Cloudflare | no memory-fork primitive or no number | Fly's `clone` copies config+volume, not memory |

### Honest caveats

- **Network position.** Our end-to-end numbers ride a tailnet to our own
  gateway; hosted providers' numbers include WAN + their API edge. Self-hosting
  earns that advantage, but it is an advantage of *position*, not implementation.
- **Hardware.** Our fleet is nested-KVM GCP VMs — bare metal (Hetzner) measured
  *faster* (cold boot ~2.2 s vs 3.5 s), so these are not best-case numbers.
- **Warm pools.** Hosted providers keep warm capacity; our hot path is a golden
  snapshot per host, built at startup. Both are "warm" strategies; neither
  column is a cold-metal number except the cold-boot rows.
- **Claims vs measurements.** Every number labeled *claim* comes from vendor
  marketing or docs without published methodology. Where an independent
  measurement exists it disagrees with the claim in every case except Modal's.

### Sources

- ComputeSDK sandbox leaderboard (independent TTI, 2026-07-17): <https://www.computesdk.com/benchmarks/sandboxes/>
- e2b: <https://e2b.dev/> · persistence docs <https://e2b.dev/docs/sandbox/persistence>
- Modal: 1M-sandboxes post (2026-07-16) <https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds> · memory snapshots <https://modal.com/blog/mem-snapshots>
- Daytona: <https://www.daytona.io/> · third-party test <https://pixeljets.com/blog/ai-sandboxes-daytona-vs-microsandbox/>
- Morph Cloud: <https://cloud.morph.so/docs/developers>
- Fly Machines: launch post <https://fly.io/blog/fly-machines/> · suspend/resume docs <https://fly.io/docs/reference/suspend-resume/>
- Vercel: snapshot-optimization post (2026-04-02) <https://vercel.com/blog/optimizing-vercel-sandbox-snapshots>
- CodeSandbox / Together: <https://www.together.ai/sandbox> · VM-clone post <https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds> · memory decompression <https://codesandbox.io/blog/how-we-scale-our-microvm-infrastructure-using-low-latency-memory-decompression>
- Cloudflare: <https://www.cloudflare.com/products/sandboxes/> · independent cold-boot study (2026-07-01) <https://alchemy.run/blog/2026-07-01-microvm-cold-starts/>
- Firecracker: <https://firecracker-microvm.github.io/>

## Reproduce

```bash
cd sdk/typescript
npm run bench:restore          # cold boot vs snapshot restore
npm run bench:fanout           # fan-out scaling
node benchmarks/burst-bench.ts --count 500 --concurrency 96 --retry-ms 250
bash ../../scripts/bench-extensive.sh   # full single-host + fleet sweep
```

E2e latency checks (hibernate wake, wake-on-connect, clock): `cd tests && npm run e2e`.
Raw run JSON lands in `sdk/typescript/benchmarks/results/` and `tests/results/`
(gitignored; each folder's README describes the files).
