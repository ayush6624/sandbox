/**
 * Shared helpers for the example scripts. Every example talks to a running
 * sandbox API server via the SDK's default env vars:
 *
 *   export SANDBOX_API_URL=http://<host>:8080
 *   export SANDBOX_API_KEY=<key>
 */

/** Exits with a helpful message unless both API env vars are set. */
export function ensureCreds(): void {
  for (const name of ['SANDBOX_API_URL', 'SANDBOX_API_KEY']) {
    if (!process.env[name]) {
      console.error(
        `Missing required environment variable ${name}.\n` +
          `Example:\n` +
          `  export SANDBOX_API_URL=http://100.99.183.74:8080\n` +
          `  export SANDBOX_API_KEY=<your-key>`
      )
      process.exit(1)
    }
  }
}

/** Prints a labelled step header. */
export function step(msg: string): void {
  console.log(`\n==> ${msg}`)
}

/** Runs an example's `main()`, printing any failure and exiting non-zero. */
export function runExample(main: () => Promise<void>): void {
  main().catch((err) => {
    console.error('\nExample failed:', err)
    process.exit(1)
  })
}
