import type { ApiClient } from './client.js'
import type { EntryInfo, ReadOpts, WriteInfo } from './types.js'

interface ApiDirEntry {
  name: string
  size: number
  mode: string
  is_dir: boolean
  mtime: string
}

/**
 * Read, write, and list files inside the sandbox. Available as `sandbox.files`.
 */
export class Files {
  constructor(
    private readonly client: ApiClient,
    private readonly sandboxId: string
  ) {}

  /**
   * Writes a file inside the sandbox, creating parent directories as needed.
   *
   * @param path Absolute path inside the guest, e.g. `/home/sandbox/app/src/App.tsx`.
   * @param data File contents as a UTF-8 string or raw bytes.
   * @returns The written path and byte count.
   */
  async write(path: string, data: string | Uint8Array): Promise<WriteInfo> {
    const body = typeof data === 'string' ? new TextEncoder().encode(data) : data
    const res = await this.client.request('PUT', `/sandboxes/${this.sandboxId}/files`, {
      query: { path },
      body,
    })
    return (await res.json()) as WriteInfo
  }

  /**
   * Reads a file from the sandbox.
   *
   * @param path Absolute path inside the guest.
   * @param opts Pass `{ format: 'bytes' }` for binary content; the default
   *             decodes the file as UTF-8 text.
   * @throws {NotFoundError} when the file does not exist.
   */
  async read(path: string): Promise<string>
  async read(path: string, opts: { format: 'text' }): Promise<string>
  async read(path: string, opts: { format: 'bytes' }): Promise<Uint8Array>
  async read(path: string, opts?: ReadOpts): Promise<string | Uint8Array> {
    const res = await this.client.request('GET', `/sandboxes/${this.sandboxId}/files`, {
      query: { path },
    })
    const bytes = new Uint8Array(await res.arrayBuffer())
    if (opts?.format === 'bytes') return bytes
    return new TextDecoder().decode(bytes)
  }

  /**
   * Lists the entries of a directory inside the sandbox.
   *
   * @param path Absolute directory path inside the guest.
   * @returns One {@link EntryInfo} per entry.
   */
  async list(path: string): Promise<EntryInfo[]> {
    const res = await this.client.request('GET', `/sandboxes/${this.sandboxId}/dir`, {
      query: { path },
    })
    const raw = (await res.json()) as ApiDirEntry[]
    return raw.map((e) => ({
      name: e.name,
      type: e.is_dir ? 'dir' : 'file',
      size: e.size,
      mode: e.mode,
      modifiedAt: new Date(e.mtime),
    }))
  }
}
