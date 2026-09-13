import { setTimeout as sleep } from 'node:timers/promises'
import type { ClientSandbox, Operation } from '../src/index.js'

/** Start each probe when its result first appears, without waiting for other items. */
export async function observeBatch({ operation, onSandbox, timeoutMs, pollIntervalMs = 100 }: {
  operation: Operation<ClientSandbox>
  onSandbox: (sandbox: ClientSandbox, index: number) => Promise<void>
  timeoutMs: number
  pollIntervalMs?: number
}): Promise<number> {
  const started = performance.now()
  const signal = AbortSignal.timeout(timeoutMs)
  const seen = new Set<number>()
  const probes: Promise<void>[] = []
  const errors: unknown[] = []
  let operationMs = 0
  try {
    while (true) {
      for (const result of operation.state.results) {
        if (!result.value || seen.has(result.index)) continue
        seen.add(result.index)
        probes.push(onSandbox(result.value, result.index).catch((error: unknown) => { errors.push(error) }))
      }
      if (operation.done) {
        operationMs = performance.now() - started
        break
      }
      await sleep(pollIntervalMs, undefined, { signal })
      await operation.refresh(signal)
    }
  } finally {
    await Promise.all(probes)
  }
  if (errors.length) throw new AggregateError(errors, 'batch probes failed')
  return operationMs
}
