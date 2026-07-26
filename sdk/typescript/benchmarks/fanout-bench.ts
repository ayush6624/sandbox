/**
 * @deprecated Use snapshot-batch-bench.ts or `npm run bench:snapshot-batch`.
 * This file remains as an automation compatibility alias.
 */
console.warn('benchmarks/fanout-bench.ts is deprecated; use snapshot-batch-bench.ts')
await import('./snapshot-batch-bench.js')
