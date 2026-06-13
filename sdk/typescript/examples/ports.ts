/**
 * Port forwarding: start a server inside the sandbox, expose its guest port to
 * a host port, and reach it from outside via getHost() / exposePort().
 *
 * Guest port 3000 is forwarded automatically at create time; any other port
 * needs an explicit exposePort(). This example serves on 8000 to show the full
 * round trip.
 *
 * Run with: npm run example:ports
 */
import { Sandbox } from '../src/index.js'
import { ensureCreds, runExample, step } from './shared.js'

/** Polls a URL until it answers (the server takes a moment to bind). */
async function poll(url: string, attempts = 20, delayMs = 500): Promise<Response> {
  for (let i = 1; i <= attempts; i++) {
    try {
      return await fetch(url, { signal: AbortSignal.timeout(3000) })
    } catch {
      await new Promise((r) => setTimeout(r, delayMs))
    }
  }
  throw new Error(`server at ${url} never responded after ${attempts} attempts`)
}

async function main(): Promise<void> {
  ensureCreds()

  const sbx = await Sandbox.create()
  step(`Sandbox ready: ${sbx.sandboxId}`)

  try {
    step('Starting a Python HTTP server on guest port 8000...')
    await sbx.files.write('/home/sandbox/www/index.html', '<h1>served from the sandbox</h1>\n')
    // Background it with output redirected to a file so it outlives the exec
    // request (the exec process group is only torn down on timeout); the brief
    // sleep lets the server bind before the command returns.
    await sbx.commands.run(
      'cd /home/sandbox/www && nohup python3 -m http.server 8000 >/tmp/http.log 2>&1 & sleep 0.5'
    )

    step('Exposing guest port 8000 to a host port...')
    const hostAddr = await sbx.exposePort(8000)
    console.log(`  reachable at http://${hostAddr}/`)

    step('Fetching it from outside the sandbox...')
    const res = await poll(`http://${hostAddr}/`)
    console.log(`  HTTP ${res.status}: ${(await res.text()).trim()}`)

    step('All forwarded ports (3000 is the auto-forwarded primary):')
    for (const p of await sbx.listPorts()) {
      console.log(`  guest ${p.guestPort} -> host ${p.hostPort}`)
    }
  } finally {
    step(`Killing sandbox ${sbx.sandboxId}...`)
    await sbx.kill()
  }
}

runExample(main)
