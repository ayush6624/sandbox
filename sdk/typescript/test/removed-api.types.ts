/**
 * Type-level assertions for APIs removed in a major version.
 *
 * Nothing here executes — the compile IS the assertion. `npm run typecheck`
 * covers `test/`, and a `@ts-expect-error` that stops erroring fails the build,
 * so this file breaks CI the moment a removed method becomes callable again.
 * (`tsconfig.build.json` only includes `src`, so this never ships in `dist/`.)
 *
 * The matching runtime assertion — that JavaScript callers, who have no
 * compile step, still get a descriptive `SandboxError` — lives in
 * `smoke.test.ts`.
 */
import { Sandbox } from '../src/index.js'
import type { FleetHostInfo, SandboxOpts } from '../src/index.js'

// Removed in 2.0.0 in favor of `new FleetClient().hosts.list()`. Both shapes an
// unchanged 1.x consumer would have written must fail to compile; annotating
// the assignment to `FleetHostInfo[]` is deliberate, since that assignment is
// exactly what a `Promise<never>`-only shim would have let through.
async function sandboxHostsIsNotCallable(opts: SandboxOpts): Promise<void> {
  // @ts-expect-error Sandbox.hosts() was removed in 2.0.0: use FleetClient.
  const withoutOpts: FleetHostInfo[] = await Sandbox.hosts()
  // @ts-expect-error Sandbox.hosts(opts) was removed in 2.0.0: use FleetClient.
  const withOpts: FleetHostInfo[] = await Sandbox.hosts(opts)
  void withoutOpts
  void withOpts
}

void sandboxHostsIsNotCallable
