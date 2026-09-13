import { readFileSync } from 'node:fs'
import { provenanceIssues } from './metadata.js'

const paths = process.argv.slice(2)
if (!paths.length) throw new Error('Usage: npm run bench:validate -- report.json [report.json ...]')
for (const path of paths) {
  try {
    const report: unknown = JSON.parse(readFileSync(path, 'utf8'))
    const issues = provenanceIssues(report)
    console.log(`${path}: ${issues.length ? issues.join('; ') : 'provenance complete (environment declarations are not independently verified)'}`)
    if (issues.length) process.exitCode = 1
  } catch (error) {
    console.error(`${path}: ${error instanceof Error ? error.message : String(error)}`)
    process.exitCode = 1
  }
}
