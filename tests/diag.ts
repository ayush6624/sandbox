/** One-off fleet diagnostic: guest DNS, pnpm shim, /home/sandbox layout. */
import { Sandbox } from '../sdk/typescript/src/index.js'

const sbx = await Sandbox.create()
console.log('sandbox', sbx.sandboxId, 'on', sbx.info.hostAddr)
const run = async (label: string, cmd: string, timeoutMs = 15_000) => {
  try {
    const r = await sbx.commands.run(cmd, { timeoutMs })
    console.log(`--- ${label} (exit 0, ${r.durationMs}ms)\n${(r.stdout + r.stderr).trim()}`)
  } catch (e) {
    const err = e as Error & { stdout?: string; stderr?: string }
    console.log(`--- ${label} FAILED: ${err.name}: ${err.message.split('\n')[0]}`)
    if (err.stdout || err.stderr) console.log((err.stdout ?? '') + (err.stderr ?? ''))
  }
}

await run('home layout', 'ls -la /home/sandbox/ && (test -d /home/sandbox/app && echo APP-DIR-EXISTS || echo APP-DIR-MISSING)')
await run('resolv.conf', 'cat /etc/resolv.conf')
await run('dns lookup', 'time getent hosts registry.npmjs.org', 10_000)
await run('which pnpm', 'which pnpm && file $(which pnpm) | head -1 && head -3 $(which pnpm)')
await run('pnpm version', 'time pnpm --version', 20_000)
await run('outbound http', 'curl -s -m 5 -o /dev/null -w "%{http_code}" https://registry.npmjs.org/ || echo CURL-FAILED', 10_000)
await sbx.kill()
console.log('done, sandbox killed')
