/**
 * @deprecated Use snapshot-source-bench.ts or `npm run bench:snapshot-source`.
 * This file remains as an automation compatibility alias.
 */
console.warn('benchmarks/restore-bench.ts is deprecated; use snapshot-source-bench.ts')
await import('./snapshot-source-bench.js')
