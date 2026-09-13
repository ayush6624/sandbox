import { setTimeout as sleep } from 'node:timers/promises'

/** Requests follow an absolute schedule even while earlier requests remain pending. */
export async function runArrivals<T>({ count, rate, task }: {
  count: number
  rate: number
  task: (arrival: { index: number; scheduledMs: number; dispatchedMs: number }) => Promise<T>
}): Promise<PromiseSettledResult<T>[]> {
  if (!Number.isInteger(count) || count < 1 || !Number.isFinite(rate) || rate <= 0) {
    throw new Error('count must be a positive integer and rate must be positive and finite')
  }
  const started = performance.now()
  const pending: Promise<PromiseSettledResult<T>>[] = []
  for (let index = 0; index < count; index++) {
    const scheduledMs = index * 1000 / rate
    while (performance.now() - started < scheduledMs) await sleep(scheduledMs - (performance.now() - started))
    const dispatchedMs = performance.now() - started
    pending.push(Promise.resolve().then(() => task({ index, scheduledMs, dispatchedMs })).then(
      value => ({ status: 'fulfilled', value }),
      (reason: unknown) => ({ status: 'rejected', reason }),
    ))
  }
  return Promise.all(pending)
}
