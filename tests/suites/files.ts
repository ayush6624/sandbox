/**
 * File API: text/binary round-trips (hash-verified), large payloads, parent
 * directory creation, directory listing, unicode, overwrites, and error
 * mapping for missing paths.
 */

import { createHash } from 'node:crypto'
import type { Sandbox } from '../../sdk/typescript/src/index.js'
import { SuiteDef, assert, assertEq, assertThrows } from '../harness.js'
import type { Ctx } from '../harness.js'

export const suite = new SuiteDef('files')

let shared: Sandbox | undefined
async function sandbox(ctx: Ctx): Promise<Sandbox> {
  if (!shared) shared = await ctx.createTracked()
  return shared
}

function sha256(data: Uint8Array | string): string {
  return createHash('sha256').update(data).digest('hex')
}

suite.test('text round-trip with exact byte count', async (ctx) => {
  const sbx = await sandbox(ctx)
  const content = 'hello sandbox\nline two\n'
  const info = await sbx.files.write('/home/sandbox/roundtrip.txt', content)
  assertEq(info.path, '/home/sandbox/roundtrip.txt', 'write reports the path')
  assertEq(info.bytes, Buffer.byteLength(content), 'write reports the byte count')
  const back = await sbx.files.read('/home/sandbox/roundtrip.txt')
  assertEq(back, content, 'text content must round-trip exactly')
})

suite.test('unicode content and spaces in paths', async (ctx) => {
  const sbx = await sandbox(ctx)
  const content = 'héllo wörld — 你好 🚀\nтест\n'
  const path = '/home/sandbox/unicode dir/ünï côde.txt'
  await sbx.files.write(path, content)
  const back = await sbx.files.read(path)
  assertEq(back, content, 'unicode content in a unicode path must round-trip')
})

suite.test('binary round-trip: all 256 byte values', async (ctx) => {
  const sbx = await sandbox(ctx)
  const data = new Uint8Array(256 * 16)
  for (let i = 0; i < data.length; i++) data[i] = i % 256
  await sbx.files.write('/home/sandbox/all-bytes.bin', data)
  const back = await sbx.files.read('/home/sandbox/all-bytes.bin', { format: 'bytes' })
  assertEq(back.length, data.length, 'binary length')
  assertEq(sha256(back), sha256(data), 'binary content must be byte-identical')
})

suite.test('large binary file (8 MiB) round-trips hash-identical', async (ctx) => {
  const sbx = await sandbox(ctx)
  const size = 8 * 1024 * 1024
  const data = new Uint8Array(size)
  // Deterministic pseudo-random fill (fast, no crypto needed for payload).
  let seed = 0x12345678
  for (let i = 0; i < size; i++) {
    seed = (seed * 1103515245 + 12345) & 0x7fffffff
    data[i] = seed & 0xff
  }
  const localHash = sha256(data)

  const start = performance.now()
  await sbx.files.write('/home/sandbox/big.bin', data)
  const writeMs = performance.now() - start

  // Verify inside the guest too, so we know the bytes on the guest disk (not
  // just the HTTP echo) are correct.
  const guest = await sbx.commands.run('sha256sum /home/sandbox/big.bin')
  assertEq(guest.stdout.split(/\s+/)[0], localHash, 'guest-side hash must match')

  const readStart = performance.now()
  const back = await sbx.files.read('/home/sandbox/big.bin', { format: 'bytes' })
  const readMs = performance.now() - readStart
  assertEq(sha256(back), localHash, 'read-back hash must match')
  ctx.log(`8MiB write=${Math.round(writeMs)}ms read=${Math.round(readMs)}ms`)
})

suite.test('write creates parent directories', async (ctx) => {
  const sbx = await sandbox(ctx)
  const path = '/home/sandbox/deep/a/b/c/leaf.txt'
  await sbx.files.write(path, 'nested')
  assertEq(await sbx.files.read(path), 'nested', 'nested write must round-trip')
  const check = await sbx.commands.run('test -d /home/sandbox/deep/a/b/c && echo yes')
  assertEq(check.stdout.trim(), 'yes', 'intermediate directories must exist')
})

suite.test('overwrite replaces content (including shrinking)', async (ctx) => {
  const sbx = await sandbox(ctx)
  await sbx.files.write('/home/sandbox/over.txt', 'a'.repeat(10_000))
  await sbx.files.write('/home/sandbox/over.txt', 'short')
  assertEq(await sbx.files.read('/home/sandbox/over.txt'), 'short', 'overwrite must truncate')
})

suite.test('list reports names, types, and sizes', async (ctx) => {
  const sbx = await sandbox(ctx)
  await sbx.commands.run('mkdir -p /home/sandbox/listing/subdir')
  await sbx.files.write('/home/sandbox/listing/file-a.txt', 'aaaa')
  await sbx.files.write('/home/sandbox/listing/file-b.bin', new Uint8Array(2048))

  const entries = await sbx.files.list('/home/sandbox/listing')
  const byName = new Map(entries.map((e) => [e.name, e]))
  assertEq(entries.length, 3, 'three entries expected')
  assertEq(byName.get('subdir')?.type, 'dir', 'subdir must be a dir')
  assertEq(byName.get('file-a.txt')?.type, 'file', 'file-a must be a file')
  assertEq(byName.get('file-a.txt')?.size, 4, 'file-a size')
  assertEq(byName.get('file-b.bin')?.size, 2048, 'file-b size')
  assert(byName.get('file-a.txt')!.modifiedAt instanceof Date, 'mtime must parse')
})

suite.test('reading a missing file raises NotFoundError', async (ctx) => {
  const sbx = await sandbox(ctx)
  await assertThrows(
    () => sbx.files.read('/home/sandbox/no-such-file.txt'),
    'NotFoundError',
    'read of a missing path'
  )
})

suite.test('listing a missing directory raises NotFoundError', async (ctx) => {
  const sbx = await sandbox(ctx)
  await assertThrows(
    () => sbx.files.list('/home/sandbox/no-such-dir'),
    'NotFoundError',
    'list of a missing directory'
  )
})

suite.test('file written via exec is readable via the API (and vice versa)', async (ctx) => {
  const sbx = await sandbox(ctx)
  await sbx.commands.run('echo via-exec > /home/sandbox/from-exec.txt')
  assertEq(
    (await sbx.files.read('/home/sandbox/from-exec.txt')).trim(),
    'via-exec',
    'exec-written file via files.read'
  )
  await sbx.files.write('/home/sandbox/from-api.txt', 'via-api')
  const res = await sbx.commands.run('cat /home/sandbox/from-api.txt')
  assertEq(res.stdout, 'via-api', 'API-written file via exec')
})

suite.test('20 concurrent writes to distinct paths all land', async (ctx) => {
  const sbx = await sandbox(ctx)
  await Promise.all(
    Array.from({ length: 20 }, (_, i) =>
      sbx.files.write(`/home/sandbox/concurrent/f-${i}.txt`, `payload-${i}`)
    )
  )
  const entries = await sbx.files.list('/home/sandbox/concurrent')
  assertEq(entries.length, 20, 'all concurrent writes must exist')
  for (let i = 0; i < 20; i++) {
    assertEq(
      await sbx.files.read(`/home/sandbox/concurrent/f-${i}.txt`),
      `payload-${i}`,
      `file ${i} content`
    )
  }
})
