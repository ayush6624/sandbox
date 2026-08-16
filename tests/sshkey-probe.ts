/**
 * ssh_pubkey create probe.
 *
 * installSSHKey is the one guest call on the create path that is NOT
 * best-effort: if it fails, the server destroys the sandbox and fails the
 * create. It is also a once-per-bring-up call, so it moved to the
 * keep-alive-free transport — and the e2e suite has no ssh_pubkey coverage.
 *
 * Runs several churn rounds so later rounds land on recycled guest IPs whose
 * previous owners are dead: the condition that broke pooled guest connections.
 *
 *   SANDBOX_API_URL=... SANDBOX_API_KEY=... npx tsx sshkey-probe.ts
 */
import { generateKeyPairSync } from 'node:crypto'
import { Sandbox } from '../sdk/typescript/src/index.js'

const ROUNDS = Number(process.env.SSHKEY_ROUNDS ?? 3)
const PER_ROUND = Number(process.env.SSHKEY_PER_ROUND ?? 4)

/** An OpenSSH-format ed25519 public key line, generated fresh per sandbox. */
function newPubkey(tag: string): string {
  const { publicKey } = generateKeyPairSync('ed25519', {
    publicKeyEncoding: { type: 'spki', format: 'der' },
    privateKeyEncoding: { type: 'pkcs8', format: 'der' },
  })
  // ed25519 SPKI DER: the trailing 32 bytes are the raw key.
  const raw = publicKey.subarray(publicKey.length - 32)
  const type = Buffer.from('ssh-ed25519')
  const blob = Buffer.concat([
    u32(type.length),
    type,
    u32(raw.length),
    raw,
  ])
  return `ssh-ed25519 ${blob.toString('base64')} ${tag}`
}

function u32(n: number): Buffer {
  const b = Buffer.alloc(4)
  b.writeUInt32BE(n)
  return b
}

const failures: string[] = []
const createMs: number[] = []

async function checkOne(round: number, i: number): Promise<Sandbox> {
  const tag = `probe-r${round}-b${i}`
  const pubkey = newPubkey(tag)
  const t0 = Date.now()
  const sbx = await Sandbox.create({ sshPubkey: pubkey })
  createMs.push(Date.now() - t0)
  try {
    // The key must have landed, with the ownership/permissions sshd demands.
    const authorized = await sbx.commands.run('cat /home/sandbox/.ssh/authorized_keys')
    if (!authorized.stdout.includes(pubkey.split(' ')[1])) {
      failures.push(`${tag}: authorized_keys missing the key we sent`)
    }
    const stat = await sbx.commands.run(
      "stat -c '%U:%G %a' /home/sandbox/.ssh /home/sandbox/.ssh/authorized_keys"
    )
    const lines = stat.stdout.trim().split('\n')
    if (lines[0] !== 'sandbox:sandbox 700') failures.push(`${tag}: .ssh perms = ${lines[0]}`)
    if (lines[1] !== 'sandbox:sandbox 600') failures.push(`${tag}: authorized_keys perms = ${lines[1]}`)
    // sshd must be listening the moment the sandbox is handed back.
    const listening = await sbx.commands.run("ss -ltn '( sport = :22 )' | tail -n +2 | wc -l")
    if (listening.stdout.trim() === '0') failures.push(`${tag}: nothing listening on :22`)
    // Unique host key per sandbox (no two sandboxes may impersonate each other).
    const hostKey = await sbx.commands.run('cat /etc/ssh/ssh_host_ed25519_key.pub')
    if (!hostKey.stdout.startsWith('ssh-ed25519 ')) failures.push(`${tag}: no ed25519 host key`)
    seenHostKeys.push(hostKey.stdout.trim().split(' ')[1])
  } catch (e: unknown) {
    failures.push(`${tag}: ${e instanceof Error ? e.message : String(e)}`)
  }
  return sbx
}

const seenHostKeys: string[] = []

function pct(xs: number[], p: number): number {
  const s = [...xs].sort((a, b) => a - b)
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))]
}

async function main(): Promise<void> {
  console.log(`ssh_pubkey probe: ${ROUNDS} rounds x ${PER_ROUND} sandboxes`)
  for (let round = 1; round <= ROUNDS; round++) {
    const boxes = await Promise.all(
      Array.from({ length: PER_ROUND }, (_, i) =>
        checkOne(round, i).catch((e: unknown) => {
          failures.push(`round ${round} box ${i} create: ${e instanceof Error ? e.message : String(e)}`)
          return null
        })
      )
    )
    await Promise.all(boxes.map((b) => b?.terminate().catch(() => {})))
    console.log(`  round ${round}/${ROUNDS} done (failures so far: ${failures.length})`)
  }

  const unique = new Set(seenHostKeys)
  if (unique.size !== seenHostKeys.length) {
    failures.push(`host keys not unique: ${seenHostKeys.length} sandboxes, ${unique.size} distinct keys`)
  }

  console.log(
    `\ncreate-with-key latency: p50=${pct(createMs, 50)}ms p95=${pct(createMs, 95)}ms n=${createMs.length}`
  )
  console.log(`distinct SSH host keys: ${unique.size}/${seenHostKeys.length}`)
  if (failures.length > 0) {
    console.log(`\nFAILURES (${failures.length}):`)
    for (const f of failures.slice(0, 20)) console.log(`  ${f}`)
    process.exit(1)
  }
  console.log('\nssh_pubkey probe passed')
}

main().catch((e: unknown) => {
  console.error('fatal:', e)
  process.exit(1)
})
