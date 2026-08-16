# Templates: your container image, booted as a microVM

A template turns a container image into a sandbox base. The image's filesystem
becomes the **guest's root filesystem** — nothing runs in a container inside the
VM; the sandbox *is* your image, with a real kernel, real init, real network.

```bash
docker build -t my-env .
sudo ./sandbox template build --from-image my-env --name my-env
# → prints a template id
```

```ts
const sbx = await Sandbox.createFromSource({ templateId: '<template id>' })
await sbx.commands.run('my-preinstalled-tool --version')
```

Every sandbox created from it is a clone of a prepared, already-booted machine,
so the cost of installing your toolchain is paid once at build time rather than
on every sandbox.

---

## What you can use it for

**Ship your own toolchain.** The default image has Node, Python, and build
tooling. If your work needs Rust, CUDA userspace, a JVM, a pinned compiler, or
40 apt packages, put them in a Dockerfile once and every sandbox starts with
them. No `apt-get` on the critical path, no first-run latency for your users.

**Make the sandbox match production.** Point the build at the same image your CI
or production containers use. What runs in the sandbox is then the same
userland, down to the library versions — which is what makes a sandbox usable
for reproducing a bug rather than approximating one.

**Amortize expensive setup.** Anything you can bake, you stop paying for: a
warmed `node_modules`, a compiled toolchain, a downloaded dataset or model
weights, a seeded database. The template captures the filesystem *after* that
work, and cloning it is roughly the cost of a snapshot restore.

**Run per-task environments at scale.** Agent benchmarks and RL environments
ship one image per task (Terminal-Bench, SWE-bench, and friends). Build a
template per task, then clone it per trial or per rollout — a group of parallel
attempts over identical initial state, which is exactly what a GRPO-style
rollout group or a sharded eval wants. Measured: 2 clones live in **1.08 s**,
identity adoption 7–12 ms each.

**Give each customer or project its own environment.** A template id is just an
id: store one per customer, per repo, or per branch, and create sandboxes
against it. Rebuild it when their dependencies change; existing sandboxes are
unaffected.

**Pin an environment in time.** A template is immutable. An image that still
built last year may not build today, but a template built from it boots
identically forever — useful for long-lived evaluation suites and for support
cases where "works on the old version" has to stay reproducible.

---

## Building one

```bash
# from a Dockerfile you have
docker build -t my-env .
sudo ./sandbox template build --from-image my-env --name my-env

# from any published image
sudo ./sandbox template build --from-image ghcr.io/acme/task-runner:2.0 --name acme

# from a flattened tar produced anywhere (`docker export` output)
sudo ./sandbox template build --from-tar ./rootfs.tar --name my-env
```

| Flag | Meaning |
|---|---|
| `--from-image` / `--from-tar` | the image to build from (exactly one) |
| `--name` | a label recorded on the template |
| `--size` | rootfs size, default `10G` (sparse — a large value is cheap) |
| `--vcpus`, `--mem-mib` | resources baked into the template (see limits) |

It prints the template id on stdout and progress on stderr, so
`TEMPLATE=$(sudo ./sandbox template build --from-image my-env)` works.

## Creating sandboxes from one

```ts
// SDK
const sbx = await Sandbox.createFromSource({ templateId })
const group = await Sandbox.createMany({ count: 16, source: { templateId } })
```

```jsonc
// API — these are equivalent; a template id IS a snapshot id
POST /v1/sandboxes  {"source": {"type": "template", "id": "<id>"}}
POST /v1/sandboxes  {"source": {"type": "snapshot", "id": "<id>"}}
```

The result is an ordinary sandbox: exec, file transfer, the interactive pty,
port forwarding, SSH, pause/resume, TTL, and usage metering all behave exactly
as they do for a default sandbox.

---

## How it works

### The build

```
docker export → extract → overlay the sandbox contract → mkfs.ext4 → boot once → snapshot
```

1. **Export.** The image is flattened to a filesystem tar (`docker create` +
   `docker export`), which drops layers, whiteouts, and everything else that is
   a container-runtime concern rather than a filesystem.
2. **Overlay.** The guest side of the sandbox contract is added to the extracted
   tree — the agent binary, an account entry, shell rc files, and drop-ins for
   whatever the image happens to ship. Details below.
3. **Format.** `mkfs.ext4 -d` builds the filesystem directly from the tree, so
   nothing has to be loop-mounted.
4. **Boot and snapshot.** The build boots that rootfs once as a real microVM,
   waits for the agent to answer, snapshots the running guest, and throws the VM
   away. Failing here means the image cannot serve as a sandbox — you find out
   at build time, not at create time.

The snapshot id it returns is the template id.

### A template is a snapshot

This is the load-bearing design decision. Snapshots already do everything a
template distribution layer needs, so templates add no new storage, no new id
space, and no new distribution path:

- **Creating from a template is fan-out** — the same primitive that makes
  ordinary creates fast, with the template as the source.
- **Templates are portable across the fleet.** A host that has never seen a
  template pulls its artifacts from the snapshot bucket on first use, so no
  scheduling has to care where a template was built.
- **Everything downstream works unchanged**, because the result is an ordinary
  sandbox — including pause/resume, which suspends a template-derived sandbox to
  disk and wakes it in tens of milliseconds.

### PID 1

A container image has no init system: its `ENTRYPOINT` is a contract with a
container runtime, not with a kernel. So the guest boots the sandbox agent as
PID 1. It mounts the pseudo-filesystems a Linux userland assumes (`/proc`,
`/sys`, `/dev`, `/dev/pts`, `/dev/shm`, `/run`), sets the hostname, takes over
ACPI shutdown so the filesystem is flushed on terminate, and then re-execs
itself as a supervised child.

The split matters: orphaned processes reparent to PID 1 and must be reaped
there, but a generic reaper living in the same process as the agent would steal
exit statuses from the agent's own child handling and break every `exec`. The
supervisor reaps anything that lands on it; the agent only ever reaps its own.

### The image's identity is honored

A container image declares how its processes run, and workloads written for it
depend on that — a benchmark verifier installs packages and checks `$PWD`, and
its solution script writes files relative to the working directory. So the build
records what the image declares, and the agent adopts it:

| From the image | Effect in the sandbox |
|---|---|
| `USER` (unset ⇒ `root`, Docker's default) | who exec, file operations, and the shell run as |
| `WORKDIR` | the default working directory for exec |
| `ENV` | exported into every login shell, so a custom `PATH` survives |

Running as root inside a template guest is consistent with the boundary the
system already relies on: isolation is the microVM — jailer, seccomp, per-sandbox
tap and disk — not the guest uid.

### Network identity without iproute2

A clone resumes holding the template's baked IP and MAC, and has to adopt its
own before it is reachable. The obvious implementation shells out to `ip`, which
would make iproute2 a hard requirement of every image — and essentially no
published image ships it. Instead the agent reconfigures the interface over
netlink directly when `ip` is absent, so an image templates unmodified.

---

## Requirements and limits

**The image needs `bash`.** Exec and the interactive shell run `bash -l`. That
is the only hard requirement, and it is checked before anything is built.

**Optional, and simply absent if the image lacks them:** OpenSSH (no sshd means
no `ssh` into sandboxes from this template; everything else is unaffected) and
`sudo` (irrelevant when the image runs as root, which is the default).

**`ENTRYPOINT`/`CMD` are not run.** A sandbox is a machine you drive, not a
process you start. Run your program with exec, or start it from the shell.

**Resources are baked in.** The virtualization layer fixes vCPUs and memory when
a snapshot is taken, and restores reject overrides — so pass `--vcpus` /
`--mem-mib` at build time, and build a second template if you need a second
size.

**Creates from a template are clone-speed, not ready-pool speed.** A default
create can be served from a pre-booted pool in milliseconds; a template create
is a fan-out clone, typically a few hundred milliseconds to about a second. Size
expectations accordingly for latency-critical arrival bursts.

**linux/amd64 only.**

## Operating notes

- The build is **root-only and host-local**: it extracts an image, builds a
  filesystem, and hands a host path to the worker beside it. It is deliberately
  not part of the tenant-facing API, and the gateway does not route it.
- Run it on a host that has `docker`, or build the tar anywhere and pass
  `--from-tar`.
- Rebuilding a template produces a **new** id. Existing sandboxes keep running
  from the old one; roll forward by pointing new creates at the new id.
- `GET /v1/templates` still describes only the host's built-in image. Templates
  you build are addressed by id — keep them wherever you keep the rest of your
  environment config.
