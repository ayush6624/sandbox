/**
 * Type-level assertions for APIs removed in a major version.
 *
 * Nothing here executes — the compile IS the assertion. `npm run typecheck`
 * covers `test/`, and a `@ts-expect-error` that stops erroring fails the build,
 * so this file breaks CI the moment a removed method becomes callable again.
 * (`tsconfig.build.json` only includes `src`, so this never ships in `dist/`.)
 *
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

// Removed in 2.6.0. These were pure forwarding aliases — three names for two
// operations — and every one of them had a `@deprecated` tag pointing at the
// name kept here.
async function deprecatedAliasesAreGone(sbx: Sandbox): Promise<void> {
  // @ts-expect-error Sandbox.kill() was removed in 2.6.0: use Sandbox.terminate().
  await Sandbox.kill('id')
  // @ts-expect-error sandbox.kill() was removed in 2.6.0: use sandbox.terminate().
  await sbx.kill()
  // @ts-expect-error sandbox.hibernate() was removed in 2.6.0: use sandbox.pause().
  await sbx.hibernate()
}

void sandboxHostsIsNotCallable
void deprecatedAliasesAreGone
