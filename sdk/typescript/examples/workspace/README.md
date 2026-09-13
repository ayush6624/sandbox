# Prepare a repository once and run independent attempts

This example prepares itsdangerous 2.2.0 at commit
`096c8d42545d3b68ea21a4f890fb2b2d8979c0bd`, installs pinned Python dependencies,
and runs its upstream tests. It saves a durable snapshot and starts independent
attempts from that environment through the public SDK. You need an API endpoint
and key, Node 18.19 or later, and a sandbox image with Git, Python 3.12, and venv.
The default development image supplies the guest tools. Preparation needs
outbound access to GitHub and PyPI.

From `sdk/typescript`, install the SDK's development dependencies and run:

```bash
npm ci
export SANDBOX_API_URL=https://your-sandbox-gateway
export SANDBOX_API_KEY=your-api-key
npm run example:workspace -- --output benchmarks/results/my-workspace.json
```

The command exits zero and prints `"passed":true` only after all 297 tests, isolation checks, and
cleanup pass. The JSON report contains the source, snapshot, and batch operation
IDs, timings, test counts, file and dependency hashes, and any failures. It is
saved at each stage so a failed run leaves diagnostics. API errors retain their
request IDs, and preparation errors retain stdout, stderr, and exit status.

Each attempt runs the upstream tests before any mutation. The example then edits
a source file in one attempt and runs the tests again. It checks every sibling
and the original source, then creates another attempt from the saved snapshot
and verifies that its files still match the original. This checks independent
filesystems; it does not exercise live network connections or async migration.

To try a larger batch, set `--count 8`. Counts are bounded to 2–32 and at most
eight creates run concurrently. A failed item stays in the report alongside the
successful results. This is a finite batch, not a sustained-arrival capacity
test. Source and attempt VMs have a 30-minute TTL as a fallback if the runner
is interrupted. Normal completion deletes only this run's tagged sandboxes
and its snapshot. Cleanup failures cause a nonzero exit.

To retain the prepared snapshot for another workflow, add `--keep-snapshot`.
The report returns its ID. It expires after one hour; successful runs still
delete all their sandbox VMs. Run another attempt from a separate process with
the returned ID:

```bash
npm run example:workspace:reuse -- <snapshot-id>
```

This prints the checked test results and deletes the new sandbox. It retains
the snapshot so you can reuse it again. To use it in your own SDK workflow:

```ts
const attempt = await client.sandboxes.create({
  source: { snapshotId: savedSnapshotId },
  ttlMs: 10 * 60_000,
})
try {
  const result = await attempt.commands.run(
    'python3 /home/sandbox/workspace/check.py',
    { timeoutMs: 120_000 },
  )
  console.log(result.stdout)
} finally {
  await attempt.terminate()
}
await client.snapshots.delete(savedSnapshotId)
```

Fresh preparation and snapshot reuse have separate timers. `fresh_through_tests`
starts at the initial create request. `prepare` includes file upload, repository
download, dependency installation, and `pip check`. Snapshot capture and the
subsequent durability wait are separate. Each attempt's command and test timers
start at batch submission, including operation polling. `all_attempts_tests`
ends after the last attempt finishes its initial tests. Isolation checks and
cleanup occur afterward.

The report records allocated workspace bytes inside the guest. These are not
the fleet's retained snapshot, memory-chunk, or object-storage bytes. Storage
reclamation remains separate launch work. Default creates may use warm capacity;
clones can reuse host caches. The example does not force a cold worker or prove
cross-worker placement.

Reports remain useful without operator metadata, but cannot pass
`npm run bench:validate -- <report>` unless the operator supplies the actual
`SANDBOX_RELEASE`, `BENCH_GUEST_IMAGE_SHA256`, `BENCH_RUNNER_REGION`, and
`BENCH_CACHE_STATE`. The runner also observes gateway and worker releases before
and after the workflow. Report validation checks those observations against the
declared release. Never fill missing provenance with a guessed value.

The saved operation ID can be inspected with `client.operations.get(id)`.
Create requests use stable keys within the run, but this example does not resume
its coordinator after a process restart. A new invocation creates a new run.
