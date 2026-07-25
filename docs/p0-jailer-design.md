# P0.2 shared jailer and cgroup design

Status: **implementation started**. This document defines the host isolation
work required by P0.2 of the
[production readiness plan](production-readiness-plan.md).

Primary references:

- [Firecracker production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [Firecracker jailer operation](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
- [Firecracker security design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)

Firecracker recommends running production VMMs through its version-matched
`jailer`. The jailer creates mount isolation and a chroot, optionally creates
or joins cgroups, exposes only `/dev/kvm` and `/dev/net/tun`, applies resource
limits, and drops to a non-privileged UID/GID before executing Firecracker.

## Why the SDK jailer is insufficient here

The pinned Go SDK's `NaiveChrootStrategy` covers only SDK-managed boots. This
repository also starts Firecracker directly for hot snapshot clones and UFFD
restore. Enabling the SDK strategy would therefore create two security classes
of sandbox.

It also hard-links the kernel, rootfs, and snapshot inputs into the jail. The
GCP fleet stores mutable VM and snapshot data on a separate XFS data disk while
the kernel and binaries live on the boot disk, so hard links cannot cover every
input. Shared snapshot files also cannot simply be chowned to a unique per-VM
UID. Finally, UFFD uses a host Unix socket that must be deliberately placed
inside the jail's visible filesystem.

The implementation will therefore use one repository-owned launcher for every
mode rather than combining the SDK jailer with separate raw-path logic.

## Launch modes and required translations

| Mode | Current implementation | Jail inputs |
| --- | --- | --- |
| cold boot | Firecracker Go SDK | kernel, per-VM rootfs, API socket, log FIFO |
| snapshot restore | Firecracker Go SDK | snapshot memory/state, per-VM rootfs, API socket, log FIFO |
| hot clone | raw Firecracker API | immutable snapshot copy, per-VM rootfs, API socket |
| UFFD restore | raw Firecracker API + host fault handler | snapshot state, per-VM rootfs, API socket, UFFD socket |

All modes must produce a prepared launch with:

- one mode-tagged request and one generated VM ID;
- a host-visible API socket path and the corresponding jail-visible path;
- an idempotent cleanup function;
- explicit staged paths for every kernel, disk, snapshot, FIFO, and UFFD input;
- the same seccomp, UID/GID, namespace, cgroup, rlimit, and logging policy.

The first implementation step introduces this shared launcher boundary without
changing runtime behavior. Direct execution remains the only implementation
until the jail preparation and allocation work below is complete.

## Jail layout

Use a root-owned, non-user-writable base. On the GCP fleet it should live on
the XFS data disk to keep large snapshot/rootfs staging reflink-capable:

```text
/mnt/sandbox-data/jailer/
  firecracker/<vm-id>/
    root/
      run/firecracker.socket
      run/firecracker-log.fifo
      run/uffd.socket
      kernel/vmlinux
      disks/rootfs.ext4
      snapshots/memory
      snapshots/state
```

The launcher creates the jail as root, stages only the files needed for that
VM, applies ownership to the assigned UID/GID, and passes jail-relative paths
to Firecracker. The server continues dialing the host-visible path beneath the
jail root.

Staging policy:

- per-VM mutable rootfs: reflink into the jail, never share a writable inode;
- kernel: root-owned copy or bind-mounted read-only input;
- snapshot memory/state: per-launch reflink/copy, read-only where Firecracker
  permits it;
- API/log FIFOs: created below the jail root and accessible from the host;
- UFFD: the host handler binds the host-visible `root/run/uffd.socket`, while
  Firecracker receives `/run/uffd.socket`.

No jailer input or parent directory may be writable by the unprivileged VMM
identity.

## Identity allocation

Production uses a bounded pool of dedicated host UIDs/GIDs, one per concurrent
VMM. Allocation must be persisted with the sandbox registry so a server crash
cannot hand an identity to a second still-running VMM.

Required states:

1. reserve UID/GID with the tap/IP/rootfs reservation;
2. prepare and chown the jail;
3. start the jailer and record its host PID plus Firecracker child PID;
4. release the identity only after process exit and jail cleanup;
5. reconcile abandoned allocations and jail directories at server startup.

Static shared `firecracker` credentials are acceptable only as a development
transition and do not satisfy P0.2.

## Cgroup v2 and resource limits

The launcher will use cgroup v2 and set, per VMM:

- `memory.max`: guest memory plus measured VMM overhead;
- `memory.swap.max=0`;
- `pids.max`;
- `cpu.max` and `cpu.weight`;
- `io.max` after resolving the backing device major/minor;
- jailer `--resource-limit no-file=...` and `fsize=...`.

The GCP workers run `serve` inside a Nomad task cgroup. Child cgroups must stay
under that allocation's delegated hierarchy; placing VMMs in a top-level
Firecracker cgroup would escape the Nomad task's aggregate memory limit.
Before enabling enforcement, the fleet gate must prove the required
controllers are delegated and that the jailer can create a leaf without
violating cgroup v2's no-internal-process rule.

Firecracker's production guidance also calls out the host `kvm-pit/<pid>`
kernel thread. The verification gate must check whether the fleet kernel
creates it and, when present, move it into the VM cgroup.

## PID and lifecycle behavior

Enable the jailer's new PID namespace. The jailer parent and Firecracker child
have different host PIDs, so the launcher must expose both explicitly:

- control and `/proc` security checks target the Firecracker child;
- lifecycle signaling normally targets the non-daemonized jailer process
  group;
- reconciliation validates executable identity before signaling either PID;
- cleanup waits for process exit before removing sockets, staged files,
  cgroups, and the UID/GID reservation.

The launcher remains non-daemonized so Go owns process lifetime and captures
bounded stdout/stderr.

## Rollout gates

1. Shared direct launcher seam, mode coverage tests, no behavior change.
2. Fleet prerequisite probe: version-matched jailer, trusted paths, cgroup v2
   controller delegation, UID/GID range, XFS jail base.
3. Jailed cold boot on a disposable worker.
4. Jailed snapshot restore and hot clone.
5. Jailed UFFD restore with the in-jail socket path.
6. Crash/reconcile and resource-exhaustion tests.
7. Make jailed launch mandatory in the production profile; retain direct mode
   only as an explicit development escape hatch.

The fleet must never run a mixed placement pool where some launch modes are
jailed and others are direct.
