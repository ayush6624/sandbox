import { randomUUID } from 'node:crypto'
import { readFileSync } from 'node:fs'

const manifest: unknown = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'))
if (!manifest || typeof manifest !== 'object' || !('version' in manifest) || typeof manifest.version !== 'string') {
  throw new Error('SDK package.json has no version')
}
const sdkVersion = manifest.version

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
  release_source: 'environment' | 'unknown'
  builds: BuildObservation[]
  environment: {
    guest_image_sha256?: string
    runner_region?: string
    cache_state?: string
  }
}

interface BuildObservation {
  target: string
  endpoint: string
  captured_at: string
  releases: Array<{ component: string; release: string; host?: string }>
  error?: string
}

/** Stable context included in every host-side benchmark result. */
export function benchmarkMetadata(
  benchmark: string,
  workload: Record<string, unknown>,
  target = process.env.SANDBOX_API_URL ?? 'unknown',
): BenchmarkMetadata {
  const release = process.env.SANDBOX_RELEASE || process.env.BENCH_RELEASE || 'unknown'
  return {
    schema_version: 2,
    benchmark,
    workload,
    run_id: process.env.BENCH_RUN_ID || randomUUID(),
    started_at: new Date().toISOString(),
    api_version: 'v1',
    sdk: {
      name: 'sandbox',
      version: sdkVersion,
    },
    release,
    release_source: release === 'unknown' ? 'unknown' : 'environment',
    target: redactTarget(target),
    builds: [],
    environment: {
      guest_image_sha256: process.env.BENCH_GUEST_IMAGE_SHA256,
      runner_region: process.env.BENCH_RUNNER_REGION,
      cache_state: process.env.BENCH_CACHE_STATE,
    },
  }
}

/** Observe running artifacts separately from the release asserted by the runner. */
export async function observeBuilds(metadata: BenchmarkMetadata, baseUrl: string, apiKey: string): Promise<void> {
  const target = redactTarget(baseUrl)
  async function observe(endpoint: string): Promise<BuildObservation> {
    const observation: BuildObservation = { target, endpoint, captured_at: new Date().toISOString(), releases: [] }
    try {
      const response = await fetch(`${baseUrl.replace(/\/+$/, '')}${endpoint}`, {
        headers: { Authorization: `Bearer ${apiKey}` }, signal: AbortSignal.timeout(5_000),
      })
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      const text = await response.text()
      for (const line of text.split('\n')) {
        const labels = /^sandbox_build_info\{([^}]+)\}\s+1(?:\.0)?\s*$/.exec(line)?.[1]
        if (!labels) continue
        const component = /(?:^|,)\s*component="([a-z]+)"/.exec(labels)?.[1]
        const release = /(?:^|,)\s*release="([a-zA-Z0-9._-]+)"/.exec(labels)?.[1]
        const host = /(?:^|,)\s*host="([a-zA-Z0-9._-]+)"/.exec(labels)?.[1]
        if (component && release) observation.releases.push({ component, release, ...(host ? { host } : {}) })
      }
      if (!observation.releases.length) throw new Error('no build identity in metrics')
      if (/^sandbox_host_scrape_ok\{[^}]+\}\s+0\s*$/m.test(text)) throw new Error('one or more workers did not answer the metrics scrape')
    } catch (error) {
      observation.error = error instanceof Error ? error.message : String(error)
    }
    metadata.builds.push(observation)
    return observation
  }
  const primary = await observe('/metrics')
  if (primary.releases.some((build) => build.component === 'gateway')) await observe('/metrics/hosts')
}

/** Promotion checks are stricter than exploratory runs, which always retain diagnostics. */
export function provenanceIssues(value: unknown): string[] {
  if (!value || typeof value !== 'object' || !('metadata' in value)) return ['missing metadata']
  const metadata = value.metadata
  if (!metadata || typeof metadata !== 'object') return ['missing metadata']
  const issues: string[] = []
  if (!('passed' in value) || value.passed !== true) issues.push('run did not pass')
  if (!('schema_version' in metadata) || metadata.schema_version !== 2) issues.push('unsupported metadata schema')
  const sdk: unknown = Reflect.get(metadata, 'sdk')
  if (!sdk || typeof sdk !== 'object' || !('version' in sdk) || typeof sdk.version !== 'string' || !/^\d+\.\d+\.\d+(?:[-+].+)?$/.test(sdk.version)) {
    issues.push('missing SDK version')
  }
  for (const key of ['run_id', 'release', 'target', 'benchmark', 'started_at']) {
    const entry: unknown = Reflect.get(metadata, key)
    if (typeof entry !== 'string' || !entry.trim() || entry === 'unknown') {
      issues.push(`missing ${key}`)
    }
  }
  const declaredRelease: unknown = Reflect.get(metadata, 'release')
  const builds: unknown = Reflect.get(metadata, 'builds')
  let workers = 0
  if (!Array.isArray(builds) || !builds.length) issues.push('missing observed build identities')
  else for (const raw of builds) {
    const observation: unknown = raw
    if (!observation || typeof observation !== 'object' || !('releases' in observation) || !Array.isArray(observation.releases)) {
      issues.push('invalid build observation')
      continue
    }
    if ('error' in observation || !observation.releases.length) issues.push('incomplete build observation')
    for (const rawRelease of observation.releases) {
      const build: unknown = rawRelease
      if (!build || typeof build !== 'object' || !('release' in build) || build.release !== declaredRelease) issues.push('observed release does not match declared release')
      if (build && typeof build === 'object' && 'component' in build && build.component === 'worker') workers++
    }
  }
  if (!workers) issues.push('missing observed worker release')
  const environment: unknown = Reflect.get(metadata, 'environment')
  if (!environment || typeof environment !== 'object') issues.push('missing declared environment')
  else {
    for (const key of ['runner_region', 'cache_state']) {
      const entry: unknown = Reflect.get(environment, key)
      if (typeof entry !== 'string' || !entry.trim() || entry === 'unknown') issues.push(`missing ${key}`)
    }
    const hash: unknown = Reflect.get(environment, 'guest_image_sha256')
    if (typeof hash !== 'string' || !/^[a-f0-9]{64}$/i.test(hash)) issues.push('missing guest image SHA-256')
  }
  return issues
}

/** Metadata attached to temporary resources for attribution and cleanup. */
export function benchmarkResourceMetadata(metadata: BenchmarkMetadata): Record<string, string> {
  return {
    benchmark: metadata.benchmark,
    benchmark_run_id: metadata.run_id,
    benchmark_release: metadata.release,
  }
}

export function redactTarget(value: string): string {
  try {
    const url = new URL(value)
    return ['http:', 'https:'].includes(url.protocol) ? url.origin : 'unknown'
  } catch {
    return 'unknown'
  }
}
