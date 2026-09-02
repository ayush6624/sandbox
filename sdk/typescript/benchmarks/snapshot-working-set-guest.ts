/**
 * Guest half of the snapshot working-set benchmark.
 *
 * `hold` creates incompressible memory and filesystem state, then continuously
 * touches every anonymous-memory page. `verify` proves that the snapshotted
 * process survived, waits for another complete memory sweep, and reads the
 * complete filesystem working set.
 *
 * Keep this file erasable-syntax-only so Node 22 can execute it directly.
 */
import { DatabaseSync } from 'node:sqlite'
import {
  closeSync,
  createReadStream,
  existsSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  writeFileSync,
  writeSync,
} from 'node:fs'
import { createHash, randomFillSync, randomBytes } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import { join } from 'node:path'

const ROOT = '/tmp/snapshot-working-set'
const PID_PATH = '/tmp/snapshot-working-set.pid'
const PAGE_SIZE = 4 * 1024
const MIB = 1024 * 1024
const sleeper = new Int32Array(new SharedArrayBuffer(4))

interface Manifest {
  runId: string
  memoryMiB: number
  memorySha256AtStart: string
  diskMiB: number
  diskSha256: string
  smallFiles: number
  sqliteRows: number
}

interface Heartbeat {
  cycle: number
  timeNs: string
}

function sleep(ms: number): void {
  Atomics.wait(sleeper, 0, 0, ms)
}

function positiveInt(raw: string | undefined, label: string): number {
  const value = Number(raw)
  if (!Number.isInteger(value) || value < 1) throw new Error(`${label} must be a positive integer`)
  return value
}

function atomicJSON(path: string, value: unknown): void {
  const temporary = `${path}.tmp`
  writeFileSync(temporary, JSON.stringify(value))
  renameSync(temporary, path)
}

function fillRandom(buffer: Buffer, hash?: ReturnType<typeof createHash>): void {
  const chunkSize = MIB
  for (let offset = 0; offset < buffer.length; offset += chunkSize) {
    const length = Math.min(chunkSize, buffer.length - offset)
    randomFillSync(buffer, offset, length)
    hash?.update(buffer.subarray(offset, offset + length))
  }
}

function writeDirtyFile(path: string, sizeMiB: number): string {
  const hash = createHash('sha256')
  const fd = openSync(path, 'w')
  const chunk = Buffer.allocUnsafe(MIB)
  try {
    for (let index = 0; index < sizeMiB; index++) {
      randomFillSync(chunk)
      hash.update(chunk)
      writeSync(fd, chunk)
    }
    fsyncSync(fd)
  } finally {
    closeSync(fd)
  }
  return hash.digest('hex')
}

function createSmallFiles(path: string, count: number, runId: string): void {
  mkdirSync(path, { recursive: true })
  for (let index = 0; index < count; index++) {
    const seed = createHash('sha256').update(`${runId}:${index}`).digest()
    const body = Buffer.allocUnsafe(PAGE_SIZE)
    for (let offset = 0; offset < body.length; offset += seed.length) seed.copy(body, offset)
    writeFileSync(join(path, `${String(index).padStart(5, '0')}.bin`), body)
  }
}

function createDatabase(path: string, sizeMiB: number): { db: DatabaseSync; rows: number } {
  const db = new DatabaseSync(path)
  db.exec('PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA synchronous=FULL')
  db.exec('CREATE TABLE IF NOT EXISTS payloads (id INTEGER PRIMARY KEY, payload BLOB NOT NULL)')
  const rows = sizeMiB * 512
  const insert = db.prepare('INSERT INTO payloads(payload) VALUES (?)')
  for (let start = 0; start < rows; start += 256) {
    db.exec('BEGIN')
    for (let index = start; index < Math.min(rows, start + 256); index++) insert.run(randomBytes(2048))
    db.exec('COMMIT')
  }
  return { db, rows }
}

function hold(argv: string[]): never {
  const memoryMiB = positiveInt(argv[0], 'memoryMiB')
  const diskMiB = positiveInt(argv[1], 'diskMiB')
  const smallFiles = positiveInt(argv[2], 'smallFiles')
  const sqliteMiB = positiveInt(argv[3], 'sqliteMiB')
  const runId = argv[4]
  if (!runId) throw new Error('runId is required')

  rmSync(ROOT, { recursive: true, force: true })
  mkdirSync(ROOT, { recursive: true })
  writeFileSync(PID_PATH, String(process.pid))

  const dirtyMemory = Buffer.allocUnsafe(memoryMiB * MIB)
  const memoryHash = createHash('sha256')
  fillRandom(dirtyMemory, memoryHash)
  const diskSha256 = writeDirtyFile(join(ROOT, 'dirty.bin'), diskMiB)
  createSmallFiles(join(ROOT, 'small'), smallFiles, runId)
  const { db, rows } = createDatabase(join(ROOT, 'state.db'), sqliteMiB)

  const manifest: Manifest = {
    runId,
    memoryMiB,
    memorySha256AtStart: memoryHash.digest('hex'),
    diskMiB,
    diskSha256,
    smallFiles,
    sqliteRows: rows,
  }
  atomicJSON(join(ROOT, 'manifest.json'), manifest)
  const sync = spawnSync('sync')
  if (sync.status !== 0) throw new Error(`sync failed with status ${sync.status}`)

  const heartbeatInsert = db.prepare('INSERT INTO payloads(payload) VALUES (?)')
  let cycle = 0
  while (true) {
    // Completion of one cycle proves that every restored anonymous page was
    // readable and writable. Change one byte per page to keep it dirty.
    for (let offset = 0; offset < dirtyMemory.length; offset += PAGE_SIZE) {
      dirtyMemory[offset] = (dirtyMemory[offset]! + 1) & 0xff
    }
    cycle++
    heartbeatInsert.run(Buffer.from(`heartbeat-${cycle}`))
    atomicJSON(join(ROOT, 'heartbeat.json'), {
      cycle,
      timeNs: process.hrtime.bigint().toString(),
    } satisfies Heartbeat)
    if (cycle === 1) writeFileSync(join(ROOT, 'ready'), 'ready')
    sleep(50)
  }
}

async function sha256File(path: string): Promise<string> {
  const hash = createHash('sha256')
  await new Promise<void>((resolve, reject) => {
    const stream = createReadStream(path, { highWaterMark: MIB })
    stream.on('data', (chunk: string | Buffer) => hash.update(chunk))
    stream.on('error', reject)
    stream.on('end', resolve)
  })
  return hash.digest('hex')
}

function heartbeat(): Heartbeat {
  return JSON.parse(readFileSync(join(ROOT, 'heartbeat.json'), 'utf8')) as Heartbeat
}

async function verify(expectedRunId: string | undefined): Promise<void> {
  if (!existsSync(join(ROOT, 'ready'))) throw new Error('ready marker is missing')
  const manifest = JSON.parse(readFileSync(join(ROOT, 'manifest.json'), 'utf8')) as Manifest
  if (expectedRunId && manifest.runId !== expectedRunId) {
    throw new Error(`run ID mismatch: got ${manifest.runId}, expected ${expectedRunId}`)
  }

  const pid = Number(readFileSync(PID_PATH, 'utf8').trim())
  try {
    process.kill(pid, 0)
  } catch (error) {
    // EPERM still proves that the PID exists; some test/container boundaries
    // deny signalling a sibling process even though both are visible.
    if ((error as NodeJS.ErrnoException).code !== 'EPERM') throw error
  }
  const firstCycle = heartbeat().cycle
  const deadline = Date.now() + 120_000
  let lastCycle = firstCycle
  while (Date.now() < deadline && lastCycle <= firstCycle) {
    sleep(50)
    lastCycle = heartbeat().cycle
  }
  if (lastCycle <= firstCycle) throw new Error('restored process did not complete another memory sweep')

  const diskSha256 = await sha256File(join(ROOT, 'dirty.bin'))
  if (diskSha256 !== manifest.diskSha256) throw new Error('large-file checksum mismatch')

  const smallFiles = readdirSync(join(ROOT, 'small')).length
  if (smallFiles !== manifest.smallFiles) {
    throw new Error(`small-file count mismatch: got ${smallFiles}, expected ${manifest.smallFiles}`)
  }

  const db = new DatabaseSync(join(ROOT, 'state.db'))
  const integrity = String((db.prepare('PRAGMA integrity_check').get() as Record<string, unknown>).integrity_check)
  const sqliteRows = Number((db.prepare('SELECT count(*) AS count FROM payloads').get() as { count: number }).count)
  db.close()
  if (integrity !== 'ok' || sqliteRows < manifest.sqliteRows) {
    throw new Error(`SQLite verification failed: integrity=${integrity}, rows=${sqliteRows}`)
  }

  console.log(JSON.stringify({
    runId: manifest.runId,
    memoryMiB: manifest.memoryMiB,
    diskMiB: manifest.diskMiB,
    smallFiles,
    sqliteRows,
    memoryCycleBefore: firstCycle,
    memoryCycleAfter: lastCycle,
  }))
}

const [mode, ...args] = process.argv.slice(2)
if (mode === 'hold') hold(args)
else if (mode === 'verify') await verify(args[0])
else throw new Error('usage: snapshot-working-set-guest.ts hold <memoryMiB> <diskMiB> <smallFiles> <sqliteMiB> <runId> | verify [runId]')
