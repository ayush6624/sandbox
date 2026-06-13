/**
 * Lifecycle & management: create with an auto-destroy timeout, extend it, list
 * running sandboxes, reconnect to one by id from a fresh handle, then kill.
 *
 * Run with: npm run example:lifecycle
 */
import { Sandbox } from '../src/index.js'
import { ensureCreds, runExample, step } from './shared.js'

const when = (d?: Date): string => d?.toISOString() ?? 'never'

async function main(): Promise<void> {
  ensureCreds()

  step('Creating a sandbox that auto-destroys in 60s...')
  const sbx = await Sandbox.create({ timeoutMs: 60_000 })
  console.log(`  ${sbx.sandboxId} — expires ${when(sbx.info.expiresAt)}`)

  try {
    step('Extending the timeout to 5 minutes...')
    await sbx.setTimeout(5 * 60_000)
    console.log(`  now expires ${when(sbx.info.expiresAt)}`)

    step('Listing running sandboxes...')
    for (const s of await Sandbox.list()) {
      const mine = s.sandboxId === sbx.sandboxId ? '  <- this one' : ''
      console.log(`  ${s.sandboxId}  ${s.status}${mine}`)
    }

    step('Reconnecting by id from a fresh handle...')
    const again = await Sandbox.connect(sbx.sandboxId)
    const who = await again.commands.run('hostname; whoami; pwd')
    console.log(who.stdout.trim().split('\n').map((l) => `  ${l}`).join('\n'))
  } finally {
    step(`Killing sandbox ${sbx.sandboxId}...`)
    await sbx.kill()
  }
}

runExample(main)
