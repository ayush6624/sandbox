# AGENTS.md

## Project overview

Firecracker-based microVM devboxes for frontend development. Spins up an Ubuntu
24.04 VM with Node 22, pnpm, TypeScript, and a Vite React-TS project
pre-installed (`node_modules` baked into the rootfs, Vite started by systemd on
boot). Lovable / e2b style — but self-hosted, on bare metal.

## Build & run

```bash
make build            # Local build (uses stub on macOS — Firecracker calls return ErrLinuxOnly)
make build-linux      # Cross-compile bin/websandbox for linux/amd64
```

Firecracker requires Linux with `/dev/kvm`. macOS is dev-only.

```bash
sudo ./websandbox doctor --config configs/devbox.json
sudo ./websandbox up    --config configs/devbox.json   # blocks until Ctrl+C or `down`
sudo ./websandbox down  --config configs/devbox.json   # from another shell
```

## Remote deployment

The Makefile defaults to `REMOTE_USER=ayush REMOTE_HOST=machine REMOTE_DIR=web-sandbox`.

```bash
make sync                 # build-linux + rsync bin/websandbox + Makefile + configs + scripts
make remote-doctor        # ssh + run doctor
make remote-up            # ssh + run up (blocks)
make remote-down          # ssh + run down
```

Note: `sync` rsyncs `bin/websandbox` so the binary lands at `~/web-sandbox/websandbox`
(not `~/web-sandbox/bin/websandbox`). All `remote-*` targets and the README use
`./websandbox`. Don't reintroduce `./bin/websandbox` in remote commands.

For driving up/down from automation (e.g. CI or a Codex session), set up
passwordless sudo scoped to the binary:

```
ayush ALL=(ALL) NOPASSWD: /home/ayush/web-sandbox/websandbox
```

in `/etc/sudoers.d/websandbox` with mode `0440`.

## One-time host setup

```bash
sudo bash scripts/setup-firecracker.sh      # install firecracker binary
sudo bash scripts/setup-kernel.sh           # download Firecracker-compatible kernel
sudo bash scripts/build-devbox-rootfs.sh    # build /opt/fc/devbox-rootfs.ext4 (resumable, ~5 min)
sudo bash scripts/setup-network.sh          # create tap0, NAT, port-forward 5173 (idempotent)
```

`setup-network.sh` is the only one that's NOT one-time per host: tap0 and iptables
rules don't survive a reboot, so it must be re-run after every host restart
(only the sysctl `net.ipv4.ip_forward=1` persists, via `/etc/sysctl.d/99-firecracker.conf`).

## Code layout

```
cmd/websandbox/
  main.go              Root cobra command (wires up/down/doctor)
  up.go                Boot VM, save state with PID, wait for signal OR VM exit, clean shutdown
  down.go              Read state, SIGTERM the firecracker PID, remove state
  doctor.go            Colored env checks (Linux, KVM, firecracker, kernel, rootfs, tap, ip_forward)
  helpers.go           Shared flags (--config, --firecracker, --kernel, --rootfs, --socket,
                       --vcpus, --mem-mib, --no-validate) and config-+-flags merge
internal/config/config.go    JSON config with Defaults() — DisallowUnknownFields
internal/state/state.go      VMState {PID, SocketPath, VMID} JSON persistence
internal/vm/
  machine_linux.go     Firecracker SDK integration, networking, snapshot-less for now
  machine_stub.go      Non-Linux stub matching the Linux signatures
  options.go           RunOptions (paths + networking) and RuntimeConfig
configs/devbox.json    Default config (tap0, 172.16.0.2/24, 2 vCPU, 1 GiB, Vite on :5173)
scripts/               Host setup shell scripts
```

## Architecture notes

- **Build tags**: `//go:build linux` for SDK code, `//go:build !linux` for the stub.
  Keep `Machine` and the public functions (`NewMachine`, `Start`, `StopForce`,
  `ShutdownGuest`, `Wait`, `PID`) signature-identical in both files.
- **Shutdown path**: `up` waits in a `select` on either `ctx.Done()` (signal) or
  `vm.Wait` completing (firecracker died — e.g. `down` SIGTERMed it). Signal path
  calls `ShutdownGuest` (ACPI), then `vm.Wait` with timeout, then `StopForce` as fallback.
  VM-exit path skips straight to state cleanup. Don't drop the VM-exit case
  — without it, `down` orphans the `up` process.
- **State file** at `/tmp/websandbox-state.json` (configurable). Single-VM only —
  the path is global, the tap is `tap0`, the guest IP is hardcoded in `devbox.json`.
  Multi-VM support is not implemented.
- **Rootfs is mutable**. Writes inside the guest persist to `/opt/fc/devbox-rootfs.ext4`.
  Running two VMs from the same image will corrupt it. For parallel VMs you need
  to copy the rootfs (or use `cp --reflink=always` on btrfs/XFS).
- **`disableValidation` arg on `NewMachine`** lets you build the SDK config on
  non-Linux for dry runs. The `--no-validate` flag wires this up.

## Conventions

- Config merging: JSON file < CLI flags. Implemented in `cmd/websandbox/helpers.go:loadAndMerge`.
- Socket paths auto-generate UUIDs when left empty.
- Use `signal.NotifyContext` for signal handling, not raw `signal.Notify` + channel.
- Commits: short imperative subject lines (see `git log`). No co-author trailer.
- Don't add unused snapshot/benchmark APIs — they were considered and explicitly dropped;
  this project is for running devboxes, not measuring them.

## Not done yet

- **`down` is single-VM** — relies on the global state file path.
- **No CoW rootfs** / per-VM rootfs copy.
- **No exec/SSH bridge** into the VM. Serial console only (root password `devbox`
  per the rootfs build script).
- **No dynamic port forwarding** — host:5173 → guest:5173 is hardcoded in `setup-network.sh`.
- **No tests.** Zero `_test.go` files.
- **Doctor warning checks** (tap, ip_forward) are warnings rather than failures,
  but `up` will still fail without them. Consider promoting to hard fails if it
  causes confusion.
