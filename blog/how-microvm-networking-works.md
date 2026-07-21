# How microVM networking works — and how we clone a running machine 50 times

*A ground-up tour of Linux networking, told through the lens of building a
Firecracker microVM sandbox. We start with how a single packet finds its way
across a wire, build up to how a virtual machine gets an IP at all, and finish
with the trick that lets us stamp out dozens of identical running VMs from one
frozen snapshot — without any two of them colliding on the network.*

> **Who this is for.** You've used Linux, maybe spun up a cloud VM or a Docker
> container, but the words *bridge*, *tap*, *ARP*, and *NAT* are fuzzy. By the
> end you'll understand exactly how a virtual machine talks to the internet,
> and why "just copy the VM" is a surprisingly deep problem. No prior
> networking theory assumed. Every concept is drawn back to how a **traditional
> Linux machine** — a laptop, a bare-metal server, a normal cloud instance —
> does the same thing.

---

## 0. The thing we're building

A **microVM sandbox**: a real virtual machine — its own Linux kernel, its own
memory, hardware-isolated — that boots in a fraction of a second and runs
untrusted code safely. Think "a container's speed with a VM's isolation." We use
[Firecracker](https://firecracker-microvm.github.io/), the same VM engine behind
AWS Lambda and Fargate.

Each sandbox needs to reach the internet (to `npm install`, `git clone`, etc.),
and we need to reach *into* it (to run commands, forward a web app's port). That
means networking. And networking is where most of the interesting Linux
fundamentals live.

The payoff question we're building toward:

> We can **freeze** a running microVM to disk (a *snapshot*) and **thaw** it
> back in ~15 ms, skipping the whole boot. So can we thaw the *same* snapshot 50
> times and get 50 running VMs? The CPU and memory part is easy. The
> **networking** part is where it gets deep — and that's the story.

Let's earn that answer from the ground up.

---

## 1. Two addresses for every machine (Layer 2 and Layer 3)

Here is the single idea that unlocks everything else: **every machine on a
network has two different addresses, used for two different jobs.**

### Layer 2 — the MAC address (the "which socket on the wall" address)

Every network interface — your laptop's Wi-Fi card, a server's NIC, a VM's
virtual NIC — is burned (or assigned) a **MAC address**: a 48-bit number like
`52:54:00:ab:cd:11`. This is its **Layer 2** identity, the "link layer."

The unit of data at L2 is a **frame**:

```
┌─────────────┬─────────────┬──────────────────────────┐
│ dst MAC     │ src MAC     │ payload (an IP packet)    │
└─────────────┴─────────────┴──────────────────────────┘
```

L2 is dumb and local. It has no concept of "the internet" or "networks" or
routing. It only knows: *this MAC, on this physical segment (this wire/switch).*
Think of a MAC address as **the specific socket on the wall** — it identifies a
physical port, nothing more.

> **Traditional Linux parallel.** Run `ip link` on any Linux box. The
> `link/ether 52:54:00:...` line is your NIC's MAC. It never changes as you move
> around the internet; it's the identity of the *hardware port* on the local
> link.

### Layer 3 — the IP address (the "postal address" address)

On top of L2 sits **Layer 3**, the network layer, where **IP addresses** live
(e.g. `172.16.0.10`). IP addresses are grouped into **subnets** written in CIDR
notation: `172.16.0.0/24` means "the addresses `172.16.0.0` through
`172.16.0.255`" (the `/24` says the first 24 bits are the network part). The unit
here is a **packet**, and a packet rides *inside* an Ethernet frame's payload.

IP is smart and global. It's the layer that knows about *other* networks and how
to *route* between them. An IP address is like a **postal address**: structured,
hierarchical, routable across the whole world.

> **Traditional Linux parallel.** `ip addr` shows your IP (`inet 192.168.1.42/24`
> on home Wi-Fi). The `/24` is your subnet. Everything in your subnet you can
> reach directly; everything else goes through a **gateway** (your router).

### ARP — the translator between the two (remember this one)

Here's the friction: when your machine wants to send an IP packet to
`172.16.0.10` *on the local subnet*, it has the postal address (IP) but the wire
only delivers by socket-on-the-wall (MAC). It needs to translate IP → MAC.

That translator is **ARP** (Address Resolution Protocol). The kernel shouts a
broadcast frame to *everyone* on the segment: **"Who has `172.16.0.10`? Tell
me your MAC."** The machine that owns that IP replies: **"That's me, my MAC is
`52:54:00:ab:cd:11`."** The asker caches the answer (you can see this cache with
`ip neigh`) and fills in the frame's destination MAC.

```
   "Who has 172.16.0.10?"  ──broadcast to all──▶  every machine on the segment
   "172.16.0.10 is at 52:54:00:ab:cd:11" ◀──unicast reply── the owner
```

**Burn this into memory.** Almost every "two machines collide on the network"
bug — including the one at the heart of VM cloning — is really an *ARP
ambiguity* or a *MAC ambiguity*. We'll come back to ARP three times.

> **Traditional Linux parallel.** When your laptop first talks to your router, it
> ARPs for the gateway IP. Run `ip neigh` and you'll see the router's IP mapped
> to its MAC. That's ARP's cache, the same on every Linux machine ever.

---

## 2. The local network: switches and the bridge

So far we have machines with two addresses. Now: how do many machines on one
segment actually exchange frames? A **switch**.

A switch is a box with many ports. When a frame arrives on a port, the switch
looks at the **destination MAC** and sends the frame only out the port where
that MAC lives. How does it know which port? It **learns**: every time a frame
comes *in* a port, it notes the **source MAC** and records "MAC X is on port 3"
in its **forwarding database (FDB)**. Unknown destinations get *flooded* to all
ports (and broadcasts like ARP always flood).

In Linux, you don't need a physical switch — the kernel has one built in, called
a **bridge**. A bridge (`br-fc` in our system) is a **software Ethernet switch**.
You "plug" virtual cables into it and it forwards frames between them, learning
an FDB exactly like the hardware version.

```
                    ┌──────── br-fc (software switch) ────────┐
   port A ──────────┤  FDB:  52:54:00:..:10  → port A         │
   port B ──────────┤        52:54:00:..:11  → port B         │
   port C ──────────┤        (unknown)       → flood all      │
                    └──────────────────────────────────────────┘
```

**The failure mode that matters later:** what if *two* ports keep announcing the
**same source MAC**? The switch's FDB flaps — "MAC X is on port A… no, port B…
no, port A…" — and frames for X get delivered to whichever port wrote the entry
most recently. Delivery becomes a coin flip. **Two machines with the same MAC on
one switch is a broken network.** (Collision #1. We're collecting these.)

> **Traditional Linux parallel.** Plug two laptops into the same dumb switch and
> they're on one L2 segment — they can ARP for each other and talk directly,
> no router involved. A Linux bridge is that switch, in software. It's also
> exactly what Docker creates (`docker0`) and what your VM hypervisor uses to put
> guests "on the LAN."

---

## 3. The virtual wire: tap devices

A physical machine has a physical NIC with a cable. A *virtual* machine needs a
*virtual* NIC with a *virtual* cable. That virtual cable is a **tap device**.

A **tap** is a virtual network interface whose "other end" is a **userspace
program** instead of a physical wire. Here's the magic:

- When the Linux kernel sends a frame *out* of tap `fc0`, that frame becomes
  **readable by whatever program holds the tap's file descriptor**.
- When that program *writes* a frame into the fd, it **appears to arrive** on
  `fc0` as if a cable delivered it.

A tap carries raw Ethernet **frames** — it's a Layer 2 device, a virtual wire.

In our system, **Firecracker (the VM process) holds the tap fd.** So the data path
for a guest's network card is, literally:

```
guest kernel's eth0  ──emits frame──▶  Firecracker reads it from tap fc0
guest kernel's eth0  ◀──delivers──── Firecracker writes a frame to tap fc0
```

The guest thinks it has a normal NIC called `eth0`. In reality, "the wire" is the
Firecracker process on the host, shuttling frames between the guest and the host
tap `fc0`. Plug `fc0` into the bridge `br-fc`, and now the guest is "on the LAN"
with every other sandbox.

> **Traditional Linux parallel.** On a bare-metal server, `eth0` is backed by a
> real NIC chip and a real cable to a real switch. In a VM, `eth0` (inside the
> guest) is backed by a *tap* on the host and a *bridge* (the software switch).
> Same abstraction — `eth0` is just an interface — but the "hardware" underneath
> is software all the way down. VPNs use the same primitive: your `tun0` from
> WireGuard is the tap's close cousin (`tun` = IP-level, `tap` = Ethernet-level).

---

## 4. Reaching the outside world: routing and NAT

Our guests live on a private subnet `172.16.0.0/24` that doesn't exist on the
real internet. So how does a guest `git clone` from GitHub, and how do we reach a
web server inside the guest? The answer is the host acts as a **router** with
**NAT** (Network Address Translation).

NAT means rewriting addresses on packets as they pass through the host. Two
directions, two flavors:

### Outbound: SNAT / MASQUERADE (guest → internet)

A packet leaves the guest with source `172.16.0.17` — an address the internet
has never heard of and can't route a reply to. So on the way out, the host
rewrites the **source** to the host's *own* public IP. This is **SNAT**
(source NAT); the dynamic form that uses "whatever the outbound interface's IP
is" is called **MASQUERADE**. Replies come back to the host, which remembers the
mapping and rewrites them back to `172.16.0.17`.

> **Traditional Linux parallel.** This is *exactly* what your home router does.
> Your laptop is `192.168.1.42` (private); the router MASQUERADEs your traffic
> behind one public IP so the whole house shares it. Every cloud VM with a
> private IP and internet access is doing the same thing. NAT is the reason
> private IP ranges (`10.x`, `172.16–31.x`, `192.168.x`) can be reused by
> everyone on earth.

### Inbound: DNAT (host port → guest port)

We want `curl http://host:5207` to reach a web app on `172.16.0.17:3000` inside
the guest. So the host rewrites the **destination** of incoming packets:
"anything for my port 5207 → actually send it to `172.16.0.17:3000`." This is
**DNAT** (destination NAT), a *port forward*.

> **Traditional Linux parallel.** This is "port forwarding" on your home router's
> admin page: "external port 8080 → 192.168.1.50:80." Identical concept. It's
> also what `docker run -p 8080:80` sets up — Docker writes a DNAT rule so a host
> port reaches a container's port.

### Where these rules live: iptables/netfilter chains

Linux applies NAT and filtering at named hook points called **chains**, as a
packet flows through the kernel:

- **PREROUTING** — rewrite a packet *before* the routing decision (for packets
  arriving from outside). DNAT for external clients goes here.
- **OUTPUT** — for packets the host *itself* generates (e.g. `curl localhost`).
  We need a DNAT rule here too, so loopback connections get forwarded.
- **POSTROUTING** — rewrite *after* routing, on the way out. MASQUERADE goes here.
- **FORWARD** — the *filter* point for packets being routed **through** the host
  (bridge ↔ internet). You must explicitly ACCEPT this traffic or the host (a
  router now) drops it.

There are two famous "silently breaks everything" sysctls we set, and now you can
understand *why*:

- `net.ipv4.ip_forward=1` — by default a Linux machine is a *host*, not a
  *router*; it won't forward packets between interfaces. Flip this on to let it
  route guest ↔ internet.
- `net.ipv4.conf.all.route_localnet=1` — by default `127.0.0.1` traffic can't
  leave the loopback interface. But our `curl localhost:5207` gets DNAT'd to a
  guest IP and must *exit* via the bridge. This sysctl permits that. Without it,
  loopback port-forwards hang forever.

> **Traditional Linux parallel.** A fresh Linux laptop has `ip_forward=0` — it's
> an endpoint, not a router. The moment you want it to *share* its connection
> (phone tethering, a VM host, a router appliance), you flip `ip_forward=1` and
> add MASQUERADE. That one bit is the difference between "a computer on the
> network" and "a computer that *is* part of the network's plumbing."

---

## 5. Putting it together: one sandbox, fully wired

Here's the whole topology for two running sandboxes on one host:

```
                          ┌───────────────── host ─────────────────────┐
                          │                                              │
  internet ── eth0(host) ─┤  iptables NAT:                               │
                          │    POSTROUTING MASQUERADE (outbound)         │
                          │    PREROUTING/OUTPUT DNAT  (inbound ports)   │
                          │                                              │
                          │         br-fc  (bridge, 172.16.0.1/24)       │
                          │            │         │                       │
                          │     ┌──────┘         └──────┐                │
                          │   fc0(tap)              fc1(tap)             │
                          └─────│─────────────────────│──────────────────┘
                                │ (Firecracker A)      │ (Firecracker B)
                          ┌─────┴──────┐         ┌─────┴──────┐
                          │ guest A    │         │ guest B    │
                          │ eth0:      │         │ eth0:      │
                          │ 172.16.0.10│         │ 172.16.0.11│
                          └────────────┘         └────────────┘
```

The bridge's own IP, `172.16.0.1`, is the **default gateway** for every guest:
when a guest wants to reach anything outside its subnet, it sends the packet to
`172.16.0.1`, and the host (router) takes over.

Let's trace **one packet each way**, using everything from Parts 1–4:

**Outbound — guest A runs `git clone github.com`:**
1. Guest A's routing table says "not local → send to gateway `172.16.0.1`."
2. Guest A **ARPs**: "who has `172.16.0.1`?" The bridge answers with its MAC.
3. Guest A builds a frame `[bridge MAC | guest A MAC | IP packet to GitHub]`,
   sends it on `eth0`.
4. Firecracker A reads it from the tap, the frame hits `br-fc`, the host routes
   it toward `eth0(host)`.
5. **POSTROUTING MASQUERADE** rewrites the source from `172.16.0.10` to the host's
   public IP. Off it goes. Replies retrace the path in reverse.

**Inbound — you run `curl http://host:5207`:**
1. **DNAT** (OUTPUT chain, since it's a local connection) rewrites the
   destination to `172.16.0.17:3000`.
2. `route_localnet` lets the now-rewritten packet leave loopback; routing sends
   it to `br-fc`.
3. The bridge forwards to `fc1`; Firecracker B delivers it to guest B's `eth0`.
4. Guest B's app replies; MASQUERADE makes the reply appear to come from
   `172.16.0.1` so guest B's return path stays inside its known subnet.

Every single step is a fundamental you now own: ARP, frames, the bridge's FDB,
routing, SNAT, DNAT.

---

## 6. How does the guest get its IP in the first place?

This question turns out to be the hinge of the whole cloning story, so we slow
down here.

### The traditional way: DHCP

On a normal Linux machine — your laptop, a cloud VM — the NIC comes up with **no**
IP. A userspace daemon (`dhclient`, `systemd-networkd`, NetworkManager) then runs
**DHCP**: it broadcasts "I need an address," a DHCP server leases it one
(`192.168.1.42`, gateway `192.168.1.1`, DNS `1.1.1.1`), and the daemon writes
that onto `eth0` and into `/etc/resolv.conf`. The lease is *renewed* periodically.
The configuration lives in **userspace** and on **disk** (lease files, config).

### The microVM way: the kernel `ip=` boot parameter

Our microVMs do something simpler and more static. There's no DHCP server and no
network daemon. Instead, the IP is handed to the guest **as a Linux kernel boot
parameter** — the same mechanism used for diskless/netboot systems for decades.

When the VM is configured, the host computes a string and passes it on the kernel
command line:

```
ip=172.16.0.10::172.16.0.1:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4:
   └─client─┘  ↑ └─gateway─┘└──netmask───┘ ↑ └if┘ ↑   └──nameservers──┘
            (no NFS server)             (no host) (off = no autoconf/DHCP)
```

The **kernel itself** — not any userspace program — parses this at boot and:
1. assigns `172.16.0.10/24` to `eth0`,
2. installs the default route via `172.16.0.1`,
3. writes the nameservers into a kernel pseudo-file, **`/proc/net/pnp`**.

Two small but crucial guest tweaks make this stick:
- We symlink `/etc/resolv.conf → /proc/net/pnp`, because glibc (and thus every
  program doing DNS) reads `/etc/resolv.conf` and has never heard of
  `/proc/net/pnp`. Without the symlink, DNS silently fails.
- We **mask** `systemd-networkd` and `systemd-resolved`, so no userspace network
  manager wakes up and clobbers the kernel's config or the resolv.conf symlink.

> **Traditional Linux parallel.** You've *seen* the `ip=` parameter without
> knowing it: it's how netboot/PXE and initramfs bring up networking before any
> userspace exists, and how diskless workstations have worked since the '90s.
> The difference from your laptop: a laptop's network identity is **soft** (a
> DHCP lease in userspace, re-negotiated constantly), while a microVM's is
> **baked in at boot by the kernel and never renegotiated.**

**Here is the consequence that everything hinges on:** because the IP is set by
the kernel at boot and then just *exists* in the running kernel's data
structures, the guest's entire network identity lives **in kernel memory** —
in RAM. There's no lease to renew, no config file being re-read. It simply *is*,
in the live kernel.

Hold that thought. We're about to freeze that RAM.

---

## 7. Freezing a running machine: snapshots

Firecracker can **snapshot** a running VM: pause it, then write its complete
state to disk so you can restore it later and resume *exactly where it left off*.
A snapshot is three artifacts:

| File | Contents | Analogy |
|---|---|---|
| `mem.bin` | the guest's **entire RAM**, byte for byte | the machine's short-term memory, frozen |
| `state.bin` | **device + CPU state**: registers, the clock, the virtual NIC's **MAC**, the disk's path, the virtio queues | the machine's "pose" — every dial and latch |
| `rootfs.ext4` | a frozen copy of the **disk** | the hard drive's contents |

**Why pause first?** For *consistency*. RAM, disk, and device registers must all
be from the same instant — otherwise RAM might reference a disk block that
changed mid-copy. Pausing the virtual CPUs freezes time so the three pieces
agree.

**Restoring is not booting.** To restore, Firecracker memory-maps `mem.bin`,
reloads the device state from `state.bin`, and unpauses the CPUs. The guest
**resumes mid-thought** — its kernel was already booted and its programs already
running when we froze it. We skip the entire boot + init + service-startup
sequence. That's why a thaw takes ~15 ms versus ~2 seconds for a cold boot.

> **Traditional Linux parallel.** This is **hibernate** (suspend-to-disk), but
> clean and external. When your laptop hibernates, it dumps RAM to disk and
> restores it on resume — your apps are right where you left them. A Firecracker
> snapshot is hibernate that the *host* controls, can copy, and can restore as
> many times as it likes. That last part is where it stops being like a laptop.

**One more fundamental — lazy memory.** Firecracker doesn't read the whole 1 GB
`mem.bin` up front. It *memory-maps* the file, so RAM pages are pulled in **on
first touch** (a page fault). A freshly restored VM might have only ~70 MB
resident even though it "has" 1 GB — it only faults in the pages it actually
uses. (This is the seam that advanced systems later exploit to *share* memory
between clones, but that's a story for another post.)

> **Traditional Linux parallel.** This is **demand paging**, the same mechanism
> that lets you run a program larger than RAM, or `mmap` a huge file and read
> only parts of it. The OS fetches pages when touched, not before. Firecracker
> applies it to an entire machine's memory image.

---

## 8. The dream, and the wall: cloning a snapshot

Now the question we set out to answer. We have a frozen, fully-booted machine
with its tools installed and a server warm in memory. We want **50 live copies**.
The CPU/memory part is genuinely easy — restore the same `mem.bin` 50 times.

But remember Part 6: **the network identity is frozen in that RAM.** And Part 7:
the **MAC** is frozen in `state.bin`. So every one of those 50 restored VMs comes
back as an *identical twin* — same IP, same MAC, all wanting the same host tap.
Put two of them on one bridge and recall our collision list:

1. **Same MAC on one bridge → FDB flap** (Part 2). The software switch can't
   decide which port owns the MAC; frames go to a random twin.
2. **Same IP on one subnet → ARP ambiguity** (Part 1). "Who has `172.16.0.10`?"
   now gets *two* answers. Whoever replies last "wins," nondeterministically.
3. **Same tap name** — two VMs literally cannot share one tap device.

This is the wall. A naive clone is a network catastrophe, and it's a catastrophe
made entirely of the fundamentals from Parts 1–2. The whole problem is L2/L3
identity collision.

> **Traditional Linux parallel.** Imagine `dd`-cloning a running server's disk
> *and* its RAM onto 50 machines on the same LAN, each booting up convinced it's
> `192.168.1.50` with the same MAC. You'd get an ARP storm and a dead network in
> seconds. Network admins know this pain as a *duplicate IP conflict* — Windows
> even pops up "another device is using this IP." Cloning machines safely has
> *always* required giving each clone a fresh identity. We just have to do it in
> milliseconds, automatically.

---

## 9. The fix: give each clone a new identity *after* it wakes up

We can't easily rewrite `mem.bin` to neutralize the baked-in identity. So instead
of *removing* the identity from the snapshot, we **defer assigning the real
identity until just after the clone resumes** — and we make sure the clone
can't talk to anyone until it has done so.

Three pieces make this work: a side-channel to whisper the new identity
(**MMDS**), a small in-guest agent that listens and reconfigures (**the thaw
hook**), and a careful **ordering** so no collision is ever visible.

### Piece 1 — MMDS: a private channel the switch never sees

Firecracker offers a **metadata service** (MMDS) — a tiny read-only HTTP endpoint
the VM process serves to *its own guest* at the link-local address
**`169.254.169.254`**.

> The `169.254.0.0/16` range is **link-local** (RFC 3927): addresses that are
> only valid on the immediate link and are never routed anywhere. If you've ever
> seen a machine with a `169.254.x.x` address, it's a NIC that failed DHCP and
> self-assigned. Cloud users know this exact IP from another angle: AWS serves
> EC2 instance metadata at `http://169.254.169.254/latest/meta-data/`. Same idea.

The property that makes MMDS perfect here: **requests to `169.254.169.254` are
intercepted by Firecracker right at the tap and answered in-process. They are
never put onto the bridge.** So a clone can fetch data over this channel and
*no other VM, and the bridge itself, ever sees a single frame of it.*

Before resuming each clone, the host pushes it a personalized document:

```json
{ "network": { "ip": "172.16.0.17", "mac": "52:54:00:ab:cd:11",
               "gateway": "172.16.0.1" } }
```

### Piece 2 — the thaw hook: the guest re-stamps its own identity

A small agent inside the guest, on waking, reads that document and reconfigures
`eth0`. Here's the sequence, with each command explained in the L2/L3 terms we
built up:

```bash
ip link set eth0 down                       # stop emitting frames at all
ip link set eth0 address 52:54:00:ab:cd:11  # new Layer-2 identity (what the bridge learns)
ip addr flush dev eth0                       # drop the stale Layer-3 identity (172.16.0.10)
ip addr add 172.16.0.17/24 dev eth0          # new Layer-3 identity
ip link set eth0 up
ip route add default via 172.16.0.1          # re-add the gateway route (flush cleared it)
```

**Why change the MAC and not just the IP?** Because all clones share *one*
bridge. Identical MACs flap the FDB (collision #1). Distinct MACs give the bridge
clean, unambiguous "this MAC → this port" entries. (If we instead gave every
clone its own isolated bridge, we *could* keep the MAC — but that throws away the
shared, simple topology from Part 5.)

**How does the guest know it was just thawed?** A resume is invisible by default —
the clock just jumps. The simplest detector: the agent polls MMDS for a
**generation number** the host bumps for each clone; when it changes, the agent
knows "I'm a fresh clone, time to re-stamp." (A `vsock` signal or a systemd unit
are alternatives.)

### Piece 3 — the ordering that makes collisions impossible

Here's the elegant part. We split "make the tap" from "plug the tap into the
bridge," and we **delay plugging in until after the clone has its new identity:**

```
1. create tap fc7  — but do NOT attach it to br-fc yet
2. resume the clone            (eth0 still believes it's 172.16.0.10 / old MAC)
3. clone reads MMDS            ✔ works — link-local, intercepted at the tap,
                                  never reaches the bridge, no one else sees it
4. clone reconfigures eth0 →   172.16.0.17 / new MAC
5. attach fc7 to br-fc         ← only NOW can its frames reach the shared switch,
                                  and by now its identity is already unique
6. add the DNAT port-forward for 172.16.0.17
```

Between steps 2 and 4, the clone *does* briefly hold the stale `172.16.0.10` and
old MAC — but **its tap isn't on the bridge**, so not one frame carrying the
stale identity can reach `br-fc`. No duplicate ARP reply. No FDB flap. By the time
the tap joins the switch (step 5), the identity is already unique and safe.

The result: each finished clone is **byte-for-byte indistinguishable from a
normally-booted sandbox** — fresh IP from the pool, fresh MAC, its own tap, on the
same bridge, reachable on its own forwarded port — but it was born in tens of
milliseconds from a frozen image instead of booting from scratch. And we reused
*all* of the host plumbing from Part 5 unchanged.

> **Traditional Linux parallel.** This is the automated, millisecond version of
> what a sysadmin does by hand after cloning a VM template: boot it on an
> *isolated* network, run a "sysprep"/`cloud-init` step that assigns a fresh
> hostname, MAC, and IP, and *only then* connect it to the real LAN. Cloud images
> do this every time you launch one — `cloud-init` reads identity from a metadata
> service (at `169.254.169.254`!) and configures the machine on first boot. We've
> just compressed "boot + cloud-init on an isolated net, then connect" into
> "resume with the tap unplugged, re-stamp from MMDS, plug in."

---

## 10. The mental model to keep

If you remember five things, remember these:

1. **Every machine has two addresses.** A MAC (Layer 2 — the socket on the wall,
   local and dumb) and an IP (Layer 3 — the postal address, global and routed).
   **ARP** translates IP → MAC on the local segment.

2. **Virtual networking is the same primitives in software.** A **tap** is a
   virtual wire (one end is a program); a **bridge** is a virtual switch; **NAT**
   on the host (MASQUERADE out, DNAT in) connects a private guest subnet to the
   world — exactly like your home router does for your laptop.

3. **A microVM's identity is baked in at boot by the kernel** (`ip=` parameter),
   not leased by DHCP in userspace — so it lives in **RAM**, and a snapshot
   freezes it there along with the MAC in device state.

4. **Cloning a snapshot naively is an L2/L3 collision disaster** — duplicate MACs
   flap the bridge's FDB, duplicate IPs make ARP ambiguous. This is the same
   "duplicate IP conflict" admins have always feared, just at machine-clone scale.

5. **The fix is deferred identity + isolation during the gap:** whisper each
   clone a new identity over the link-local **MMDS** channel the switch never
   sees, let the guest **re-stamp its own `eth0`** on thaw, and keep its **tap
   off the bridge until it's done** — so a collision is never visible to anyone.

Networking stops being magic the moment you internalize those two addresses and
the handful of devices that move frames between them. Everything else —
containers, VMs, VPNs, clouds, and yes, cloning a running machine 50 times — is
just those same fundamentals, recombined.

---

*If you found this useful, the companion piece goes equally deep on the
**storage** side: how copy-on-write filesystems (reflink/XFS) let 50 clones share
one 2 GB disk image and diverge only as they write — and the surprisingly tricky
problem of a disk path baked into a snapshot.*
