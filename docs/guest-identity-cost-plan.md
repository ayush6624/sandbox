# Removing guest-side identity work from the create path

Status: proposed (2026-08-17). Prerequisite landed: identical jailed inputs share
one inode (`2e6ba08`), which moved the fanout bottleneck off disk I/O and onto
guest CPU. This document is about the guest CPU.

## Why this is now the ceiling

Measured per clone at low load on release `2e6ba08`:

| phase | cost | what it is |
|---|---|---|
| **reidentify** | **~474 ms** | guest polls MMDS (200 ms tick), flushes `eth0`, re-adds the address, broadcasts GARP; host waits on an ARP listener |
| **identity** | **~148 ms** | remove inherited host keys, `ssh-keygen -t ed25519`, restart sshd and wait for a new listener |
| everything else | ~100 ms | jailer prepare, snapshot load, drive patch, MMDS, resume, agent poll, clock |

So **~85% of a clone is the guest doing work about its own identity.** And it is
CPU-bound work: a 32-way fanout runs at ~58% guest CPU with ~4% idle and
iowait ~0-1%. 32 guests cannot do that in the time 8 take on 16 cores, which is
why fanout is still ~linear past N≈8 and why raising `fanoutParallelism` makes
reidentify *worse* (it already breaches the first 1.5 s margin at 16-way).

Part 2 is guest-side and shipped through the image-pinned path
(`bake-image.sh bake && golden` plus a MIG roll). **Part 1 turned out NOT to be**
— see finding 5 below: the existing baked agent already leaves its address alone
when the MMDS document carries no `gen`, so Part 1 ships through ordinary
`rollout.sh`. That was an assumption in the first draft of this document, and it
was wrong.

## Part 1: stop re-identifying the guest (netns per VM)

**Today.** Every clone resumes holding the snapshot's baked IP. The tap is kept
off the bridge until the in-guest thaw agent adopts a fresh IP/MAC from MMDS and
announces it by gratuitous ARP; `finishClone` bridges on the announce, with a
1.5 s margin and a second margin as a fallback.

**Proposal.** Give every guest the *same* baked IP and disambiguate on the host:
one network namespace per VM, a veth pair (or `tc-redirect-tap`) with a unique
host-side address, and NAT between them. The guest never learns it moved.

What that deletes outright:

- the MMDS identity document and the thaw agent's 200 ms poll
- `eth0` flush/re-add in the guest, and `cmd/sandboxd/garp_linux.go`
- `provisioner.ListenARP`, `WaitForIdentity`, `reidentifyMargin`, the
  second-margin retry, and `PrimeGuestNetwork`
- the unbridged-tap dance and the "two guests briefly share the baked IP"
  hazard the whole mechanism exists to avoid
- `CloneParams.Prefix` and the three-site agreement between bridge CIDR,
  cold-boot CIDR and clone reidentify prefix

Identity becomes host-side syscall work, which parallelizes — and the host has
idle CPU once I/O stopped dominating. Expected effect: remove ~474 ms of the
~722 ms per-clone floor.

**This is the standard Firecracker pattern** (the firecracker-go-sdk's CNI
integration and `tc-redirect-tap` exist for exactly it) and the netlink library
is already in the module graph via those deps.

### Verified on hardware (2026-08-18), before writing code

Measured on a fleet worker (n2-standard-16), 16 namespaces each with NAT + clamp:

| | |
|---|---|
| setup | **42.5 ms** per namespace (0.680 s / 16, sequential `ip` forks) |
| teardown | 28.4 ms per namespace |
| egress from the ns, source 172.16.0.2 | HTTP 301 in **41 ms** |
| two namespaces egressing CONCURRENTLY from the SAME guest IP | both succeed |
| host → guest fixed IP via the ns veth address | reachable (DNAT in-ns) |
| UDP/53 from the guest's fixed source | rcode=0, real answers |

So 42.5 ms of host syscalls replaces 367-447 ms of guest work, and it is host
CPU (idle) rather than guest CPU (saturated).

**Five things the experiment settled that were NOT safe to assume.** Three of
them broke my first attempt outright:

1. **The host's `mangle FORWARD` MSS clamp does not cover forwarding inside a
   namespace** — measured 0 clamp rules in a fresh ns, while the veth is MTU 1500
   against a 1460 host path. Every namespace needs its own clamp or every guest
   re-acquires the exact problem the host clamp exists to fix. `xt_TCPMSS` does
   load in-ns.
2. **The host MASQUERADE is scoped `-s <guest subnet>` and so does not cover the
   veth range.** Without a rule for it, ns egress leaves with an unroutable
   source and silently times out — that is how the first attempt failed.
3. **Host→guest cannot use DNAT on the host**: `PREROUTING` never sees
   host-originated traffic. The DNAT belongs inside the namespace.
4. **`jailer` v1.15.0 supports `--netns`** (confirmed against the binary on a
   worker, not the docs), so the VMM can join the namespace holding its tap.
5. **This needs NO guest-agent change and NO image rebake.** `runThawAgent`
   (cmd/sandboxd/thaw.go) only reconfigures `eth0` when the MMDS document carries
   a non-empty `gen`, and it handles `epoch_ms` on an independent branch. A clone
   launched with an **epoch-only** MMDS document therefore keeps its baked
   address and never announces, while still getting its clock stepped. So this
   ships through ordinary `rollout.sh`, and the GARP/reidentify code in the guest
   becomes dead-but-harmless, deletable later in a routine bake. This contradicts
   the "both items are guest-side" claim at the top of this document.

### Landed (`029c4cd`)

- `internal/provisioner/netns.go` — `CreateNetns`/`DeleteNetns`/`ListNetns`/
  `VethEndpoints`/`EnsureVethEgress`, carrying the exact command sequence proven
  above.
- `internal/vm/launcher.go` `LaunchRequest.NetnsPath` + `internal/vm/jailer.go`
  passing `--netns`. Inert until a caller sets it.
- `internal/server/reconcile.go` — per-row namespace teardown beside the tap, plus
  an orphan sweep of `/var/run/netns`. Landed BEFORE any creator so the first
  deploy cannot leak; refuses to sweep if the row list is unavailable, since it
  could not then distinguish an orphan from a live sandbox.
- `internal/provisioner/netns_test.go` — pins the address math against collision
  over 120 consecutive allocations, the 15-byte interface-name limit, and the
  three rules above (SNAT, in-ns DNAT, per-ns clamp).

**Key simplification that keeps the remaining diff small:** `VethEndpoints`
reuses the existing guest-IP pool as the **host-side** address, so every consumer
of `sb.GuestIP` — the agent dial in `proxy.go`, `portproxy.go`'s dial,
`waitForAgent`, `syncGuestClock`, `installSSHKey`, `initializeGuestIdentity` —
keeps working untouched. Only the *meaning* of that address moves: from one
configured inside the guest to one on the host end of the veth. The plan's
worry about rewriting those call sites does not survive that choice.

### What remains

1. `EnsureNetwork` calls `EnsureVethEgress` with the veth subnet (needs a config
   value for it — currently the only uncalled piece).
2. Cold boot (`createCold`): fixed `GuestCIDR`, `CreateNetns` instead of
   `CreateTap`, pass `NetnsPath` into `RunOptions`.
3. Clone (`bringUpClone`): `CreateNetns`, drop `ListenARP`, drop
   `CreateTapUnbridged`, and send MMDS **without** `ip`/`mac`/`gen` — `epoch_ms`
   only, which is what makes the existing agent leave its address alone.
4. `finishClone`: skip the reidentify wait, the bridge attach and
   `PrimeGuestNetwork` in netns mode. This is where the 367-447 ms actually
   disappears.
5. Hibernation wake: check whether same-identity and clone-path wake really
   converge once the address is stable — plausible, unverified.
6. Once deployed and settled: delete `garp_linux.go`, `ListenARP`,
   `WaitForIdentity`, `reidentifyMargin` and the second-margin retry in a bake.

Steps 2-4 are the ones that move the number, and none of them are testable
without hardware — which is why they are deliberately not in `029c4cd` rather
than committed unverified.

## Part 2: defer the SSH host key until SSH is used

**Today** (`initializeGuestIdentity`, cmd/sandboxd/identity.go), every
independent create removes inherited host keys, generates a fresh Ed25519 key,
and restarts sshd. The Ed25519 keygen itself is ~7 ms; the cost is the
`ssh-keygen` fork plus `restartSSHService`, which SIGHUPs sshd and then polls
`/proc/<pid>/fd` + `/proc/net/tcp` every 1 ms for up to 500 ms waiting for the
listener inode to change. Measured 148 ms idle, 685 ms at 16-way.

Most sandboxes never use SSH, so this is per-clone work spent on nothing.

**The invariant that makes this non-trivial.** Deleting the key files is not
enough: sshd is already running and **already loaded the golden's host key into
memory**, so it would keep serving it. The eager restart is precisely what stops
one sandbox impersonating another. Any deferral must first ensure **no listener
is serving an inherited key**.

**Proposal.**
1. At create, keep removal eager — inherited `/etc/ssh/ssh_host_*` and
   `authorized_keys` — and additionally **stop the inherited listener** by
   signalling the pid in `/run/sshd.pid` (a syscall, microseconds, no poll). No
   keygen, no reload-and-poll.
2. Generate the host key lazily on the first SSH provisioning call — sandboxd's
   `POST /ssh-key`, which both `ssh_pubkey`-at-create and
   `PUT /v1/sandboxes/{id}/ssh-access` already route through — then start sshd
   and gate on the listener exactly as `restartSSHService` does now.
3. Keep the durable identity marker so retries stay idempotent, and record
   whether a key has been generated so step 2 runs once.

**Consequence to accept:** `:22` no longer listens the instant a guest boots —
which `build-devbox-rootfs.sh` deliberately arranged by disabling socket
activation. First SSH use pays ~150 ms. That is the trade: move a fixed cost off
every create onto the rare request that needs it.

**Risks to design for.** Under systemd, killing sshd may trigger a respawn that
then fails with no host key and can hit start limits — `restartSSHService`
already carries `reset-failed` handling for restored guests, and the stop path
needs the equivalent. Template guests have no service manager at all
(`initMode()` → `restartOwnSSHD`), so both paths need the same treatment. And a
guest image without `ssh-keygen` must keep succeeding, as it does today.

## Order, and what to measure

1. **Part 2 first.** Smaller, self-contained, and it is the cheaper half of the
   same image rebake. Expected: per-clone ~722 ms → ~575 ms, and a much smaller
   tail under burst (the 685 ms at 16-way disappears).
2. **Then Part 1**, which is the structural one and worth its own measurement
   pass.

For each: re-run `snapshot-batch-bench.ts --counts 1,2,4,8,16,32` from the
control VM, and record `vmstat` guest-CPU share during the 32-way run. The
success criterion is not just a lower N=32 number — it is that **per-sandbox time
stops rising with N**, which is the actual goal.

## What is measured vs. projected

**Measured** (release `2e6ba08`, fleet worker, 2026-08-17): the phase table
above; 32-way fanout at ~58% guest CPU / ~4% idle / ~0-1% iowait; fanout
operation wall 1017/511/509/1012/1515/3027 ms for N=1/2/4/8/16/32; per-sandbox
764 ms → 102 ms; 43.1 MiB PSS per resident ready VM with all 8 sharing one
snapshot-memory inode.

**Projected:** every latency figure attributed to Part 1 or Part 2 above. They
follow from the phase table, but neither change has been built. Expect the second
one to disappoint relative to the arithmetic — that has happened twice in this
investigation already (reflink cost, then CPU), and both times the cheap direct
experiment settled it faster than reasoning did.
