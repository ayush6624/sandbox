# Templates: booting a container image as a sandbox

A template lets you say "give me a sandbox that already has *my* toolchain in
it" instead of taking the fleet's base image and installing things per sandbox.
It is built from a container image — a `Dockerfile` you already have, or a
published reference — and the image's filesystem becomes the **guest's rootfs**.
Nothing runs in a container inside the VM; the sandbox *is* your image.

```bash
docker build -t my-env .
sudo ./sandbox template build --from-image my-env --name my-env
# → prints a snapshot id: that is the template id
```

Then create sandboxes from it through the ordinary API:

```jsonc
POST /v1/sandboxes
{"source": {"type": "snapshot", "id": "<template id>"}}
```

## Why a template is a snapshot

Snapshots already do everything a template distribution layer has to do, so
templates add no storage, no placement, and no new distribution path:

- **Creating from one is fan-out**, the same primitive that makes hot creates
  hot. Measured on a dev host: 3 clones from one template in **305 ms total**,
  reidentify **7–11 ms** each.
- **They are fleet-portable.** A worker that has never seen the template pulls
  its artifacts from the snapshot bucket on first use
  (`ensureSnapshotLocal`, `internal/server/snapshot_gcs.go`).
- **Hibernation, wake, TTL, ports, SSH, usage metering** all work unchanged,
  because the result is an ordinary sandbox. Measured: freeze 1.2 s, wake 53 ms.

So there is no template registry, no template CRUD, and no new id space. The
snapshot id *is* the template id, and `--name` is recorded on the snapshot.

## What the build does

```
docker export → extract → overlay the sandbox contract → mkfs.ext4 → boot once → snapshot
```

The overlay is what turns a container filesystem into a machine the fleet can
operate (`overlayTemplateRootfs`, `cmd/sandbox/template.go`):

| Added | Why |
|---|---|
| `/usr/local/bin/sandboxd` | the agent behind exec, files, the pty shell, and SSH |
| a `sandbox` account in the image's `/etc/passwd` | sandboxd resolves it **by name** and drops every operation to it |
| `/home/sandbox/app` + `.profile`/`.bashrc` | the default cwd and shell setup |
| `/etc/profile.d/zz-sandbox-template.sh` | re-exports the image's `ENV`, so a custom `PATH` survives into every exec |
| `/etc/hostname`, `/etc/hosts` | a resolvable hostname (sudo and friends warn without one) |
| `sudoers` / `sshd_config` drop-ins | only when the image actually ships `sudo` / OpenSSH |

The build then boots that rootfs once, waits for the agent, snapshots the
running guest, and destroys it. Failing there means the image cannot serve as a
sandbox, and you find out at build time rather than at create time.

## Requirements on the image

Two, both checked before anything is built:

- **`bash`** — `/exec` and the pty shell run `bash -l`.
- **`iproute2`** (`ip`) — a clone adopts its network identity by reconfiguring
  `eth0` at thaw. Without it the clone resumes still holding the template's
  address and is simply unreachable.

```dockerfile
RUN apt-get update && apt-get install -y --no-install-recommends bash iproute2
# alpine: apk add --no-cache bash iproute2
```

Optional, and absent-by-default in slim images:

- **OpenSSH** — no sshd means `sandbox ssh` does not work for this template.
  Everything else (exec, files, pty, ports) is unaffected.
- **sudo** — without it the workload cannot become root inside its own guest.

## What is NOT carried over from the image

- **`ENTRYPOINT` / `CMD` are not run.** A sandbox is a machine you drive, not a
  process you start. Run your program with exec, or start it from a template
  boot of your own making.
- **Any init system in the image is ignored.** sandboxd runs as PID 1: it mounts
  `/proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm`, `/run`, supervises the agent,
  and reaps orphans (`cmd/sandboxd/init_linux.go`).
- **`vcpus`/`mem_mib` are baked in at build time.** Firecracker bakes them into
  the snapshot and restores reject overrides, so pass `--vcpus`/`--mem-mib` to
  the build and make a second template if you need a second size.
- **linux/amd64 only.**

## Operational notes

- The build is **worker-local and root-only**: it extracts an image and builds a
  filesystem, and it names a host path when it asks the worker to boot it. The
  route (`POST /templates/build`) is deliberately not one the gateway proxies,
  so it is not reachable by a tenant.
- Run it on a host that has `docker`, or hand it an already-flattened tar from
  anywhere with `--from-tar` (`docker export` output).
- `--size` (default 10G) is the rootfs size. It is sparse, and per-sandbox
  copies are reflinks on XFS, so a large number here is cheap.
- Template creates are **fan-out speed (~300 ms–1 s), not ready-pool speed
  (~12 ms)**. The ready pool and the golden snapshot are per-host and singular;
  a template does not get one. Size expectations accordingly.
