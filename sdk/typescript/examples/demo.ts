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

const HELLO_TSX = `export default function Hello() {
  return (
    <div style={{ fontFamily: 'sans-serif', textAlign: 'center', marginTop: '4rem' }}>
      <h1>Hello from websandbox!</h1>
      <p>This component was written into the microVM via the SDK.</p>
    </div>
  )
}
`

const APP_TSX = `import Hello from './Hello'

function App() {
  return <Hello />
}

export default App
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

async function waitForVite(host: string, attempts = 30, delayMs = 2000): Promise<void> {
  const url = `http://${host}/`
  for (let i = 1; i <= attempts; i++) {
    try {
      const res = await fetch(url, { signal: AbortSignal.timeout(5000) })
      if (res.ok) {
        const html = await res.text()
        console.log(`Vite responded with HTTP ${res.status} after ${i} attempt(s)`)
        console.log(`First 120 chars of HTML:\n${html.slice(0, 120)}`)
        return
      }
      console.log(`Attempt ${i}/${attempts}: HTTP ${res.status}, retrying...`)
    } catch {
      console.log(`Attempt ${i}/${attempts}: not ready yet, retrying...`)
    }
    await new Promise((r) => setTimeout(r, delayMs))
  }
  throw new Error(`Vite dev server at ${url} did not respond after ${attempts} attempts`)
}

async function main(): Promise<void> {
  requireEnv('WEBSANDBOX_API_URL')
  requireEnv('WEBSANDBOX_API_KEY')

  step('Creating sandbox (boots a Firecracker microVM, ~2s)...')
  const sbx = await Sandbox.create()
  console.log(`Sandbox ready: ${sbx.sandboxId}`)

  try {
    step('Running: node --version && pnpm --version')
    const versions = await sbx.commands.run('node --version && pnpm --version')
    console.log(versions.stdout.trim())
    console.log(`(took ${versions.durationMs} ms)`)

    step('Writing src/Hello.tsx and overwriting src/App.tsx...')
    const helloInfo = await sbx.files.write('/home/sandbox/app/src/Hello.tsx', HELLO_TSX)
    console.log(`Wrote ${helloInfo.path} (${helloInfo.bytes} bytes)`)
    const appInfo = await sbx.files.write('/home/sandbox/app/src/App.tsx', APP_TSX)
    console.log(`Wrote ${appInfo.path} (${appInfo.bytes} bytes)`)

    step('Reading back src/Hello.tsx...')
    const content = await sbx.files.read('/home/sandbox/app/src/Hello.tsx')
    console.log(content)

    step('Listing /home/sandbox/app/src...')
    const entries = await sbx.files.list('/home/sandbox/app/src')
    for (const e of entries) {
      console.log(`${e.mode}  ${String(e.size).padStart(8)}  ${e.type.padEnd(4)}  ${e.name}`)
    }

    step('Polling the Vite dev server...')
    const host = sbx.getHost(5173)
    console.log(`Preview URL: http://${host}/`)
    await waitForVite(host)

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
