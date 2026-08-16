/**
 * End-to-end SSH login probe.
 *
 * sshkey-probe proves the key LANDS; this proves a user can actually log in:
 * create with ssh_pubkey → expose guest :22 through the userspace port proxy →
 * real ssh with the matching private key. Also asserts the two properties the
 * design promises: login is the unprivileged `sandbox` user (uid 1000), and
 * root login is refused outright.
 *
 * Must run somewhere that can reach the worker's host port directly (the
 * gateway is an HTTP reverse proxy and never routes raw TCP) — i.e. the control
 * VM, not a laptop.
 *
 *   SANDBOX_API_URL=... SANDBOX_API_KEY=... npx tsx ssh-login-probe.ts
 */
import { execFileSync, execFile } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { promisify } from 'node:util'
import { Sandbox } from '../sdk/typescript/src/index.js'

const execFileAsync = promisify(execFile)

const dir = mkdtempSync(join(tmpdir(), 'sandbox-ssh-probe-'))
const keyPath = join(dir, 'id_ed25519')

function sshArgs(host: string, port: number, user: string): string[] {
  return [
    '-i', keyPath,
    '-p', String(port),
    // Host keys are unique per sandbox but host ports are recycled from a pool,
    // so scope trust to the sandbox rather than host:port (see CLAUDE.md).
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', 'CheckHostIP=no',
    '-o', `UserKnownHostsFile=${join(dir, 'known_hosts')}`,
    '-o', 'BatchMode=yes',
    '-o', 'ConnectTimeout=10',
    `${user}@${host}`,
  ]
}

async function main(): Promise<void> {
  execFileSync('ssh-keygen', ['-t', 'ed25519', '-N', '', '-f', keyPath, '-C', 'probe'], {
    stdio: 'ignore',
  })
  const pubkey = readFileSync(`${keyPath}.pub`, 'utf8').trim()

  const sbx = await Sandbox.create({ sshPubkey: pubkey })
  console.log(`sandbox ${sbx.sandboxId} on ${sbx.info.hostAddr ?? '(local)'}`)
  try {
    await sbx.exposePort(22)
    // getHost() names the API endpoint, which on a fleet is the GATEWAY — and
    // the gateway is an HTTP reverse proxy that never routes raw TCP. Take the
    // port from it but dial the worker that actually owns the sandbox.
    const port = Number(sbx.getHost(22).split(':').pop())
    const host = (sbx.info.hostAddr ?? '127.0.0.1').replace(/^https?:\/\//, '').split(':')[0]
    if (!Number.isFinite(port) || port <= 0) throw new Error(`bad host port from getHost: ${port}`)
    console.log(`ssh target ${host}:${port}`)

    // Retry briefly: the listener is bound on expose, but sshd may still be
    // accepting its first connection.
    let out = ''
    let lastErr: unknown
    for (let attempt = 0; attempt < 10; attempt++) {
      try {
        const r = await execFileAsync('ssh', [...sshArgs(host, port, 'sandbox'), 'whoami; id -u; hostname'])
        out = r.stdout
        break
      } catch (e: unknown) {
        lastErr = e
        await new Promise((r) => setTimeout(r, 1000))
      }
    }
    if (!out) throw new Error(`ssh never succeeded: ${String(lastErr)}`)

    const [user, uid] = out.trim().split('\n')
    console.log(`  logged in as ${user} (uid ${uid})`)
    if (user !== 'sandbox') throw new Error(`logged in as ${user}, want sandbox`)
    if (uid !== '1000') throw new Error(`uid ${uid}, want 1000`)

    // A command round-trip over SSH, not just a login.
    const echo = await execFileAsync('ssh', [...sshArgs(host, port, 'sandbox'), 'echo SSH_EXEC_OK'])
    if (!echo.stdout.includes('SSH_EXEC_OK')) throw new Error('ssh command round-trip failed')
    console.log('  ssh command round-trip ok')

    // root must be refused by sshd itself.
    let rootRefused = false
    try {
      await execFileAsync('ssh', [...sshArgs(host, port, 'root'), 'whoami'])
    } catch {
      rootRefused = true
    }
    if (!rootRefused) throw new Error('root login succeeded — PermitRootLogin should refuse it')
    console.log('  root login refused')

    console.log('\nssh login probe passed')
  } finally {
    await sbx.terminate().catch(() => {})
    rmSync(dir, { recursive: true, force: true })
  }
}

main().catch((e: unknown) => {
  console.error('fatal:', e instanceof Error ? e.message : e)
  rmSync(dir, { recursive: true, force: true })
  process.exit(1)
})
