# Sandbox client documentation bundle

This directory is a portable, Mintlify-ready documentation bundle for the
TypeScript SDK. Its resource collections and lifecycle actions use `/v1`; its
command, file, and PTY handles currently use established non-versioned
execution routes outside the public OpenAPI document. It deliberately does not
add a Mintlify configuration to this repository.

## Use in a Mintlify repository

1. From the Mintlify site's configured content root (the directory containing
   `docs.json`), copy this directory as `client/`. For example:
   `cp -R sandbox/docs/client ./client`. The supplied page paths then remain
   `client/<page>`.
2. Merge the `navigation` value from
   [docs-navigation.fragment.json](docs-navigation.fragment.json) into the
   destination `docs.json`. It uses Mintlify's current `navigation.groups`
   format. If the destination nests this bundle below a tab, put these groups
   in that tab's `groups` array instead.
3. If the destination includes an API-reference page, publish or copy the
   source OpenAPI specification from `api/openapi.yaml` and configure the
   destination route for it. This bundle intentionally has no hard-coded link
   to a sibling API-reference route.
4. Replace example deployment URLs and package-install instructions with the
   distribution details for the destination deployment. If the directory is
   relocated or renamed, update both MDX links and `docs.json` page paths.

The pages use only standard MDX frontmatter, fenced code blocks, Markdown
tables, Mermaid fences, and Mintlify's `Note`, `Tip`, `Warning`, `Tabs`, and
`Tab` components.

## Contents

| Page                                             | Purpose                                         |
| ------------------------------------------------ | ----------------------------------------------- |
| [Introduction](introduction.mdx)                 | API choice, concepts, and supported runtime     |
| [Quickstart](quickstart.mdx)                     | Create, run, and terminate a sandbox            |
| [Configuration](configuration.mdx)               | Credentials, URLs, and request settings         |
| [Lifecycle and timeouts](lifecycle-timeouts.mdx) | States, TTL, idle pause, and time budgets       |
| [Commands and streaming](commands-streaming.mdx) | Buffered and NDJSON-streaming command execution |
| [Files](files.mdx)                               | Read, write, and list guest files               |
| [PTY](pty.mdx)                                   | Interactive shells and WebSocket requirements   |
| [Networking](networking.mdx)                     | HTTP URLs, worker host ports, and raw TCP       |
| [Snapshots and batches](snapshots-batches.mdx)   | Reusable state and `createMany` operations      |
| [Errors and retries](errors-retries.mdx)         | Typed errors, idempotency, and safe retries     |
| [SDK reference](sdk-reference.mdx)               | Export and collection inventory                 |
| [Migration](migration.mdx)                       | Moving from the compatibility `Sandbox` facade  |
| [Fleet operator](fleet-operator.mdx)             | Separately authenticated `FleetClient` usage    |

<Note>
The `Sandbox` static facade is retained for compatibility. New integrations
should start with `SandboxClient`: its resource collections target `/v1`, while
the attached execution handles retain their current non-versioned routes.
</Note>
