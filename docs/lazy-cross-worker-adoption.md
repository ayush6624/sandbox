# Lazy cross-worker adoption

Cross-worker adoption can start a jailed VM from a captured memory-chunk manifest. The destination downloads device state and the root filesystem, then fetches RAM through userfaultfd as the guest touches it. `uffd_restore` selects this path for chunked hibernations. `uffd_chunk_gcs` enables durable chunk publication.

The source normalizes incremental hibernation memory against its local immutable base during background publication. It publishes content-addressed chunks and a complete manifest before the durable record. This moves memory assembly out of the destination's startup path. The local sparse snapshot and its base marker remain available for same-worker wake. Older durable records with `mem_form: diff` still use eager reconstruction.

`vm.StartCloneUFFD` shares the file clone's paused-load sequence. It remaps the host tap, patches the root filesystem, writes the new network identity to MMDS, resumes, and starts working-set prefetch. The target fetches only the base root filesystem when reconstructing a disk diff; it does not fetch base RAM as a side effect. The jail contains the UFFD socket and snapshot state, with no staged RAM image for a remote chunk source.

Each running VM retains the manifest it loaded. Later faults use those content hashes even after the adoption HTTP request ends or hibernation metadata changes. Cache hits and fetched chunks must match their SHA-256 and expected size. A malformed source or unservable fault stops the affected VM instead of leaving it hung. After the guest becomes mutable, this is a VM failure, not recovery from the old checkpoint.

UFFD clones currently produce full subsequent snapshots. They do not retain a local full-memory baseline or claim differential-capture support. The worker [chunk cache](chunk-cache.md) now has a tested local budget and eviction policy, pending deployment. Remote content-addressed chunks still have no garbage collector.

Released checkpoints now also support [peer transfer](peer-hibernation-transfer.md).
That path fetches memory and artifacts from a retained source generation, with
cloud fallback. The measurements below predate peer hibernation support.

## Verification

The corrected dev release is `fb09662-dev-20260906093103`, with
`uffd_restore: true`. Three roundtrips passed on each backend. Median adoption
was 31.491 seconds with File and 2.466 seconds with UFFD outbound; return
medians were 35.255 and 2.708 seconds. This compares loaders on the same new
chunked publication format. The old diff-object protocol remains unmeasured.
Publication still took roughly 44–48 seconds in the tested workload. A separate
roundtrip preserved complete SHA-256 hashes of 256 MiB retained RAM and an
8 MiB disk file, plus process identity and timer progress.

The focused tests cover deferred chunk loading, cold rootfs-only base download, chunk integrity, memory geometry, captured-manifest lifetime, clone API ordering, and cleanup after source or process failure. Linux-specific tests exercise the UFFD protocol and fault loop in addition to the portable server tests.

Live results and commands are recorded in [the verification directory](verification/lazy-adoption-2026-09-06/). The comparison reuses the existing working-set guest fixture. It reports durability wait, adoption through agent readiness, first command, and complete state verification separately. The fixture continuously touches its allocated memory, so these observations describe an active working set rather than retained idle RAM.

The jailed snapshot writer reserves against complete guest RAM for UFFD guests, even when only part is currently resident. Snapshot creation can fault the rest into memory. Its reclaim threshold includes that future resident RAM plus bounded write-cache space; otherwise the kernel can spend minutes throttling against anonymous RAM that cannot be reclaimed. The first live run exposed this failure, and its pressure counters and stack are retained with the verification results.
