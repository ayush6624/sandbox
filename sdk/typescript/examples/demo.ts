/**
 * End-to-end demo of the websandbox SDK.
 *
 * Requires a running websandbox API server plus:
 *   export WEBSANDBOX_API_URL=http://<host>:8080
 *   export WEBSANDBOX_API_KEY=<key>
 *
 * Run with: npm run example
 */
// When installed from npm this would be: import { Sandbox } from 'websandbox'
import { Sandbox } from '../src/index.js'

const SCRIPT_PY = `import sys

print(f"Hello from websandbox, running Python {sys.version.split()[0]}")
print("This script was written into the microVM via the SDK.")
`

function requireEnv(name: string): void {
  if (!process.env[name]) {
    console.error(
      `Missing required environment variable ${name}.\n` +
        `Example:\n` +
        `  export WEBSANDBOX_API_URL=http://100.99.183.74:8080\n` +
        `  export WEBSANDBOX_API_KEY=<your-key>`
    )
    process.exit(1)
  }
}

function step(msg: string): void {
  console.log(`\n==> ${msg}`)
}

async function main(): Promise<void> {
  requireEnv('WEBSANDBOX_API_URL')
  requireEnv('WEBSANDBOX_API_KEY')

  step('Creating sandbox (boots a Firecracker microVM, ~2s)...')
  const sbx = await Sandbox.create()
  console.log(`Sandbox ready: ${sbx.sandboxId}`)

  try {
    step('Checking the toolchain: node, pnpm, python3...')
    const versions = await sbx.commands.run('node --version && pnpm --version && python3 --version')
    console.log(versions.stdout.trim())
    console.log(`(took ${versions.durationMs} ms)`)

    step('Writing a Python script into the sandbox...')
    const scriptInfo = await sbx.files.write('/home/sandbox/script.py', SCRIPT_PY)
    console.log(`Wrote ${scriptInfo.path} (${scriptInfo.bytes} bytes)`)

    step('Reading it back...')
    const content = await sbx.files.read('/home/sandbox/script.py')
    console.log(content)

    step('Running it: python3 /home/sandbox/script.py')
    const run = await sbx.commands.run('python3 /home/sandbox/script.py')
    console.log(run.stdout.trim())

    step('Listing /home/sandbox...')
    const entries = await sbx.files.list('/home/sandbox')
    for (const e of entries) {
      console.log(`${e.mode}  ${String(e.size).padStart(8)}  ${e.type.padEnd(4)}  ${e.name}`)
    }

    step('Listing all running sandboxes...')
    const sandboxes = await Sandbox.list()
    for (const s of sandboxes) {
      console.log(`${s.sandboxId}  ${s.status}  guest=${s.guestIp}  hostPort=${s.hostPort}`)
    }
  } finally {
    step(`Killing sandbox ${sbx.sandboxId}...`)
    await sbx.kill()
    console.log('Sandbox destroyed.')
  }

  console.log('\nDemo complete.')
}

main().catch((err) => {
  console.error('\nDemo failed:', err)
  process.exit(1)
})
