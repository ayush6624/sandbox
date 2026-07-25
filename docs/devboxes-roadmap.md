# Ona-style devboxes

Status: **design note, not started.**

This document records a path from the current sandbox API to persistent,
repository-aware development environments similar to
[Ona environments](https://ona.com/docs/ona/environments/overview). It is a
product layer on top of the existing Firecracker runtime, not a replacement for
it.

## Goal

A developer chooses a project and Git ref, waits for provisioning, then connects
with VS Code, Cursor, or SSH to an isolated machine that already contains the
repository, its tools, and its dependencies. The environment can stop when idle,
resume later, and expose development servers through stable preview URLs.

The intended model is:

```text
project + Git ref + environment configuration
                       |
                       v
              isolated Firecracker VM
                       |
                       v
           repository + tools + secrets
                       |
                       v
             SSH / editor / browser IDE
```

Ona uses a similar split: a management plane coordinates projects and
environments, while runners provision isolated VMs; a Dev Container inside each
VM defines the project tooling. Desktop editors connect over SSH. See Ona's
[architecture](https://ona.com/docs/ona/understanding/architecture),
[Dev Container guide](https://ona.com/docs/ona/configuration/devcontainer/getting-started),
and [editor integration](https://ona.com/docs/ona/editors/overview).

## What already exists

The repository already supplies most of the compute and lifecycle substrate:

- Fast Firecracker VM creation from a golden snapshot
- Per-sandbox mutable rootfs copies
- Multi-host placement through the gateway
- Command execution, files, and interactive PTYs
- SSH public-key injection and an in-guest SSH server
- Explicit TCP port forwarding with wake-on-connect
- Idle hibernation and hard deletion TTLs
- Snapshots, restore, and identity-neutral fan-out
- Per-sandbox CPU and memory selection

That is enough to use a sandbox as a manual devbox today:

```bash
sudo ./sandbox up \
  --name my-project \
  --ssh-key ~/.ssh/id_ed25519.pub \
  --hibernate-after 1800

sudo ./sandbox expose <sandbox-id> 22
ssh -p <host-port> root@<worker-address>
```

Inside the guest:

```bash
cd /home/sandbox/app
git clone git@github.com:owner/project.git .
pnpm install
```

For a fleet, the gateway routes the HTTP control API but not SSH traffic. The
client must currently reach the owning worker and its allocated port. A public
SSH path and stable browser previews are covered by
[the public ingress design](public-ingress.md).

## Missing product layer

A sandbox is currently an infrastructure primitive. A devbox also needs:

- Users, organizations, projects, and authorization
- Git provider authentication and repository checkout
- Project-defined, reproducible setup
- Provisioning state and streaming setup logs
- Safe runtime secret injection
- A stable workspace identity independent of the current VM or worker
- One-click editor or generated SSH configuration
- Preview URLs
- Retention, quotas, policy, and audit history
- Prebuild selection and invalidation

These concerns should live in a separate **workspace control plane**. The
low-level sandbox API should remain useful to SDK consumers and agents without
gaining GitHub- or editor-specific behavior.

```text
dashboard / CLI / editor extension
                  |
                  v
         workspace control plane
         - identity and projects
         - Git provider access
         - workspace lifecycle
         - secrets and policy
         - setup state and logs
                  |
                  v
          sandbox gateway API
                  |
                  v
          Firecracker workers
```

## Workspace API sketch

The exact API is undecided. A minimal creation request could be:

```http
POST /workspaces
```

```json
{
  "project_id": "proj_123",
  "git_ref": "feature/login",
  "machine_class": "standard",
  "ttl_sec": 14400
}
```

A workspace record should map the user-facing identity to the current sandbox
and owning worker without exposing those implementation details as its primary
identity:

```json
{
  "id": "ws_123",
  "project_id": "proj_123",
  "git_ref": "feature/login",
  "status": "ready",
  "sandbox_id": "2fdcea66-...",
  "created_at": "2026-07-25T12:00:00Z",
  "last_active_at": "2026-07-25T12:10:00Z"
}
```

Useful lifecycle states:

```text
queued -> creating -> cloning -> configuring -> ready
                                      |
                                      +-> failed

ready -> hibernated -> ready
ready -> deleting -> deleted
```

Creation would:

1. Authorize access to the project and requested Git ref.
2. Create a sandbox with the selected resources and an ephemeral provisioning
   credential.
3. Clone the repository into `/workspaces/<repository>`.
4. Apply the project's environment configuration and stream its logs.
5. Remove one-time Git credentials.
6. Inject user SSH access and runtime secrets.
7. Expose configured services.
8. Mark the workspace ready and return editor connection information.

## Phase 1: useful workspace MVP

The first milestone is:

> Create a workspace from a repository and branch, run its setup, and return a
> working SSH target.

Implement:

- `POST`, `GET`, list, stop/resume, and delete operations for workspaces
- GitHub App integration, with short-lived installation tokens
- Clone and checkout into `/workspaces/<repository>`
- Streaming provisioning logs and durable failure messages
- A non-root `devbox` user with deliberate, limited sudo access
- Per-user SSH keys and automatic SSH exposure
- A CLI command that writes or prints an SSH config entry
- Idle hibernation, deletion TTLs, and organization resource limits
- A small native project configuration format

An intentionally small initial format avoids blocking the MVP on full Dev
Container compatibility:

```yaml
# .sandbox/devbox.yaml
image: default

setup:
  - corepack enable
  - pnpm install

services:
  web:
    command: pnpm dev --host 0.0.0.0
    port: 5173
```

The orchestrator should make each setup step observable and retryable. A failed
setup should leave the environment reachable in a recovery mode rather than
hiding the VM and its logs.

## Phase 2: Dev Container compatibility

For compatibility with Ona, Codespaces, and existing repositories, support
`.devcontainer/devcontainer.json`.

The likely implementation is Docker plus the Dev Container CLI inside the
microVM:

1. Clone the repository into the VM.
2. Build or pull the configured container image.
3. Mount the workspace into the development container.
4. Run Dev Container lifecycle hooks.
5. Apply the configured container user, environment, features, and mounts.
6. Install configured editor extensions or return them as editor metadata.
7. Let editor connections attach to the inner container.

This deliberately comes after the native MVP. Full compatibility includes
Docker Compose, features, mounts, user mapping, lifecycle hooks, rebuild
semantics, recovery behavior, and editor customizations.

## Phase 3: prebuilds

The current snapshot and fan-out support is a strong base for prebuilds:

```text
clone repository
  -> build environment
  -> install dependencies
  -> warm caches and indexes
  -> remove credentials and secrets
  -> snapshot
  -> fan out ready-to-code workspaces
```

A prebuild key should include at least:

```text
repository
+ base commit
+ environment configuration hash
+ rootfs/image version
+ CPU architecture
```

Branches can reuse a recent base-branch prebuild and then fetch and check out
their requested ref. Prebuild invalidation must be explicit and observable;
silently using an incompatible image is worse than a slower cold setup.

No user or repository credential may be captured in snapshot memory or disk.
Secrets must be applied only after a clone resumes. Processes that authenticated
during prebuild must be stopped or have their credential state scrubbed before
the snapshot is published.

## Later product capabilities

- Browser VS Code through OpenVSCode Server or code-server
- Stable wildcard preview URLs such as
  `https://5173-ws_123.dev.example.com`
- Dotfiles and personal shell/editor settings
- Shared workspaces and access grants
- Organization machine classes, quotas, and policies
- Audit logs and cost/usage reporting
- Scheduled and commit-triggered prebuilds
- Pull request, issue, webhook, and scheduled workspace creation
- Human and coding-agent sessions in the same environment

## Security requirements

The following are prerequisites for multi-user or internet-facing operation:

- Do not use root as the normal interactive user.
- Generate unique SSH host keys per workspace. The current base image contains
  shared host keys inherited by golden-snapshot clones.
- Never bake Git tokens, user secrets, agent credentials, or cloud credentials
  into a rootfs image or prebuild snapshot.
- Use short-lived Git provider credentials and remove them after checkout.
- Do not put credentials in Git remote URLs, shell history, setup logs, or
  process arguments.
- Replace the fleet-wide client bearer token with user authentication and
  per-workspace authorization.
- Check authorization on every workspace, shell, file, port, snapshot, and
  lifecycle operation; workspace IDs are not secrets.
- Put public SSH behind a controlled TCP edge or bastion rather than exposing
  worker networks directly.
- Apply disk, CPU, memory, process, network, port, and API limits.
- Separate user-facing ingress from the management gateway so user traffic
  cannot starve placement or heartbeat traffic.

## Recommended next step

Build the workspace orchestrator as a separate service with one vertical flow:

```text
GitHub project + ref
  -> create sandbox
  -> clone repository
  -> run .sandbox/devbox.yaml setup
  -> expose SSH
  -> return SSH config
```

That proves the product boundary and makes the existing runtime useful to a
developer. Dev Containers, prebuilds, browser editors, and richer policy can
then be added without rewriting the Firecracker substrate.
