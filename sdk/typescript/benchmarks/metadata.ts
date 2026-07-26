export interface BenchmarkMetadata {
  schema_version: 2
  benchmark: string
  workload: Record<string, unknown>
  run_id: string
  started_at: string
  api_version: 'v1'
  sdk: { name: 'sandbox'; version: string }
  release: string
  target: string
}

/** Stable context included in every host-side benchmark result. */
export function benchmarkMetadata(
  benchmark: string,
  workload: Record<string, unknown>,
): BenchmarkMetadata {
  return {
    schema_version: 2,
    benchmark,
    workload,
    run_id: process.env.BENCH_RUN_ID ?? `standalone-${process.pid}`,
    started_at: new Date().toISOString(),
    api_version: 'v1',
    sdk: {
      name: 'sandbox',
      version: process.env.npm_package_version ?? '1.0.0',
    },
    release: process.env.SANDBOX_RELEASE ?? process.env.BENCH_RELEASE ?? 'unknown',
    target: redactTarget(process.env.SANDBOX_API_URL ?? 'unknown'),
  }
}

/** Metadata attached to temporary resources for attribution and cleanup. */
export function benchmarkResourceMetadata(metadata: BenchmarkMetadata): Record<string, string> {
  return {
    benchmark: metadata.benchmark,
    benchmark_run_id: metadata.run_id,
    benchmark_release: metadata.release,
  }
}

function redactTarget(value: string): string {
  try {
    const url = new URL(value)
    return `${url.protocol}//${url.host}`
  } catch {
    return value
  }
}
