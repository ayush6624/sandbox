/**
 * Streaming exec: receive stdout/stderr chunks as they're produced (instead of
 * one buffered blob at the end), and see how a non-zero exit surfaces.
 *
 * Run with: npm run example:streaming
 */
import { Sandbox, CommandExitError } from '../src/index.js'
import { ensureCreds, runExample, step } from './shared.js'

async function main(): Promise<void> {
  ensureCreds()

  const sbx = await Sandbox.create()
  step(`Sandbox ready: ${sbx.sandboxId}`)

  try {
    step('Streaming a command that emits output over ~2s...')
    // Passing onStdout/onStderr switches commands.run() to the streaming
    // endpoint: the callbacks fire as chunks arrive, while the resolved
    // result still carries the full buffered output and timing.
    const result = await sbx.commands.run(
      'for i in $(seq 1 5); do echo "stdout line $i"; echo "stderr line $i" >&2; sleep 0.4; done',
      {
        onStdout: (chunk) => process.stdout.write(`  [out] ${chunk}`),
        onStderr: (chunk) => process.stderr.write(`  [err] ${chunk}`),
      }
    )
    step(
      `Finished in ${result.durationMs} ms (exit ${result.exitCode}); ` +
        `buffered stdout was ${result.stdout.length} bytes`
    )

    step('A non-zero exit rejects with CommandExitError...')
    try {
      await sbx.commands.run('echo "doing work"; echo "oops" >&2; exit 7')
    } catch (err) {
      if (!(err instanceof CommandExitError)) throw err
      console.log(
        `  caught CommandExitError: exit ${err.exitCode}, ` +
          `stderr=${JSON.stringify(err.stderr.trim())}`
      )
    }
  } finally {
    step(`Killing sandbox ${sbx.sandboxId}...`)
    await sbx.kill()
  }
}

runExample(main)
