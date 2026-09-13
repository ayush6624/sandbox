# Remote chunk storage design

The catalog, fenced payload writer, and per-generation collector are implemented
and tested locally. Server handoff writers and readers use the protocol behind a
new-format writing gate. Automatic collection is not enabled. The
[bounded worker cache](chunk-cache.md) is a separate completed implementation
unit awaiting fleet rollout. This design records the proposed remote ownership
protocol and the conditions that still prevent fleet collection.

## Ownership unit

Use an immutable payload prefix for each checkpoint generation. Keep its
publisher, durable-root, and reader holders in one generation catalog. A
generation-conditioned write updates the holder set and lifecycle together.
Removing the last holder makes the generation retired in that same write.
Retired generations cannot acquire new holders.

The September 13 inventory measured about 22% more compressed bytes if each
observed manifest set stored its chunks separately. Those sets include history.
That cost buys a collection unit whose payload does not depend on references
from unrelated generations. It is not a forecast of active retention cost.

We rejected the initial separate-reference protocol after an executable model
found this race. A collector closes a generation and scans its references. A
publisher moves protection from an intent object to a durable attachment behind
the listing cursor. The collector misses both and deletes data still referenced
by a committed root. A corrected transfer protocol passes its bounded model,
but adds transfer states and scan rules. The single catalog avoids that scan.

## Publication, readers, and collection

1. Create the catalog with a publisher holder before any payload request.
2. Attach the durable-root holder before exposing the root. A peer-backed root
   can be exposed before backup completes because its immutable peer source and
   publisher journal remain retained.
3. A reader obtains the root, then conditionally adds its unique holder to the
   live catalog while that root's holder remains. A stale write retries. Root
   retirement can make acquisition fail; it cannot invalidate a returned reader.
4. Release the publisher only after publication and all uploads have resolved.
   Root retirement must not release the backup publisher on its behalf.
5. Retire the durable root before removing its holder. Preserve the existing
   ownership fence or tombstone so an old publication retry cannot restore it.
6. Release each reader after its foreground and background loads have drained.
   The final holder removal retires the catalog atomically.
7. A collector conditionally changes retired to deleting, reclaims the immutable
   payload set, then records deleted. A successor resumes a persisted deletion.
   Catalog tombstones and generation identities are never reused.

GCS object generations are unique tokens; their numeric values need not increase.
An observed control generation different from the original expected generation
fences that original CAS. Publication and cleanup compare identity, not numeric
ordering. Tests exercise a replacement whose generation is numerically smaller.
[Object metadata](https://docs.cloud.google.com/storage/docs/metadata).

An unresolved PUT remains a publication obligation even after confirmed process
death. It may still complete at the provider. Matching bytes from a later read
do not prove another timed-out request has finished. The proposed write fence
first creates a zero-byte placeholder. Every nonempty upload and retry then uses
that placeholder's exact positive generation. A collector removes placeholders
as well as payloads and retries when a replacement makes its delete fail.

GCS makes generation-conditioned requests fail when the current generation no
longer matches; generation zero tests for absence of a live object.
[Request preconditions](https://docs.cloud.google.com/storage/docs/request-preconditions).
Our inference is that deleting a placeholder invalidates every pending payload
write tied to it. A delayed create-only request can recreate only an empty object.
A dead producer cannot learn its new generation and authorize another payload.
This requires authoritative producer death or a complete request drain, plus a
catalog fence preventing successors from resuming retired work.

The separate write-fence model passes 918 states. It permits late empty-object
residue and does not claim immediate zero object count or zero storage cost.
Transport checks and complete writer coverage remain prerequisites for enabling
collection. Existing unconditional uploads do not have this protection.

On September 13, the opt-in GCS media-upload test passed on the development worker
with its attached identity. It rejected payload writes tied to a deleted or
replaced placeholder, rejected a stale delete, preserved the committed bytes,
then confirmed the test key absent after cleanup. The test used
`_verification/storage-write-fence/a20f5152-d0cf-425d-8d88-795aae7235cf/payload`.
It did not modify a checkpoint or deploy a service. Historical versions and soft
deletion were not inspected. Streaming upload finalization and in-flight request
interleavings still need provider integration proof. The sparse-stream test now
pauses a body transfer, deletes and replaces its placeholder, and checks that the
old upload fails. It passes against the local GCS test server. Copying its binary
to the configured development VM was rejected by automatic approval review;
that provider test remains pending explicit test-copy authorization.

## Implemented handoff integration

`owned_handoff_storage` defaults to false. Deploy reader support to every worker
before enabling it on producers. Readers accept the new format regardless of
this writing setting. Version 3 manifests bind a handoff generation to its
control-root identity. Older workers reject version 3, and new workers reject
owned manifests placed under legacy stable hibernation keys.

New handoff payloads live under `chunksets/data/<generation>/`: gzip memory chunks,
sparse state and rootfs streams, and a final `record.json` descriptor. The
`raw-sha256` manifest codec describes raw memory hashes without precomputed
compressed lengths; the stored cloud chunks still use gzip. Catalogs live at
`chunksets/catalog/<generation>.json`, outside the deletion prefix.

The local handoff journal precedes catalog creation. A publisher and root are
attached before the offer control CAS. Backup attempts use empty placeholders
and positive-generation writes for every payload. Failed attempts drain and
suspend while retaining the publisher for retry. Cleanup releases the publisher
only after the journal confirms both publication and backup completion. It uses
the persisted offer even when a previous cleanup already removed local files.

An exclusive attempt now covers the job reread, upload, writer drain, completion,
and cleanup across Server instances and processes sharing the registry. Registry
closure cannot release an active attempt. After process exit, a successor resumes
an unfinished backup from retained source; a completed job enters cleanup without
opening another writer. Owned uploads compare the retained descriptor with the
immutable journal offer before issuing payload requests.

The registry stores at most 64 permanent lock-file identities. A deterministic
hash maps generations to these files; unrelated generations that share a file
retry on contention. Initialized files are never replaced or unlinked. Missing,
replaced, or symlinked files leave the job protected. The callback holds its own
close-on-exec descriptor until its producers drain, independently of the registry
connection lifetime. Peer acknowledgments remain available during upload, and
cloud cleanup does not hold the local peer-file lock.

This protocol requires every process sharing the registry to use the attempt
lock. Old producers must drain or exit before enabling the new behavior. An
unrecoverable source still leaves an unfinished publisher protected. Automatic
cancellation of that obligation needs a separate authority and retention policy.
The [publisher verification record](verification/chunk-publishers-2026-09-13/README.md) contains the failure reproductions, race results, and Linux restart checks.

New owned backups record each payload's accepted GCS generation in
`backup-receipt.json` before writing the unchanged final `record.json`. Immutable
retries return the exact generation compared against retained source. If local
source files disappear before the registry records completion, recovery requires
the matching journal descriptor, a complete receipt, and an unreleased publisher.
It conditionally reads bounded metadata and checks every expected payload's
current generation against its receipt. It then completes the existing journal
and cleanup without uploading payloads again. An incomplete receipt, replaced
object, canceled check, or released publisher leaves the obligation pending.

This proof relies on [GCS generations identifying immutable object data](https://docs.cloud.google.com/storage/docs/objects). It does
not authenticate coordinated administrative rewrites of receipts and payloads.
Diff backups also record the current base-rootfs generation and reject its later
loss or replacement. They retain the existing assumption that the immutable base
ID names the correct permanent base; the receipt does not establish equality to
the local base before capture. Older backups can acquire receipts through normal
source-verified retry while retained files exist. Without source and receipts,
they remain unresolved. See the
[cloud recovery verification record](verification/chunk-cloud-recovery-2026-09-13/README.md).

Peer reconstruction acquires a reader after validating the descriptor and before
requesting payloads. Lazy faults and background hydration hold separate readers;
eager reconstruction releases its reader after materialization. The VM callback
releases lazy ownership only after load workers drain. Failed staging closes
untransferred readers. Cloud fallback obtains its own reader before downloading
the generation's artifacts. Empty placeholders do not count as backup commits.

Destroy and replacement retire the old root after the successful authority CAS.
Replacement and destruction persist a pending root check in the registry before
issuing the request, so a lost
response followed by another claim does not forget the previous root. Retries
read the control and require proof that its original version was replaced and
its generation is no longer usable. Claims and reopen operations within the
same generation retain that root.
Running controls retain their root because an unsuccessful guest start may need
to reopen the same checkpoint. Root retirement preserves existing readers and
unfinished backup publishers. Root checks survive server restart and stay in
SQLite until the remote catalog confirms retirement. Replays preserve the first
observed control revision and cannot downgrade a confirmed fence. If journaling
fails, the server does not issue the authority-changing request. Queue processing
advances past timed-out entries so one unavailable root cannot starve the rest.
Retiring an absent root in an open catalog writes a retirement receipt. A delayed
attachment with the old catalog version must retry and then fails against that
receipt. Already terminal catalogs preserve their phase. This prevents a late
attachment from leaving an orphan hold after its cleanup journal was removed.

The registry writer explicitly uses SQLite `synchronous=FULL`. Tests inspect the
active connection setting and recover a committed retirement record after a
child process exits without closing SQLite. Server tests cover restart after
committed and unapplied control changes, and retry after remote retirement
succeeded but local completion failed. These tests do not simulate disk loss or
physical power failure. [SQLite synchronous](https://www.sqlite.org/pragma.html#pragma_synchronous).

Reader acquisition now starts with a prepared identity and a committed SQLite
obligation. Only then can it write the catalog. Failed acquisition retains the
same handle for release; successful acquisition stays active until its existing
consumer-drain callback runs. A confirmed remote close precedes removal of the
local obligation. If local completion fails, the closed handle retries only that
remaining step. Closing an absent reader still requires a confirmed catalog CAS,
which fences a delayed acquisition request that captured an older generation.

Each registry has a process incarnation whose exclusive lock descriptor survives
`Registry.Close`, server shutdown, and cancellation. Recovery checks the saved
lock file's device and inode, obtains its lock, and commits a death receipt before
releasing readers. Missing or replaced files without a receipt leave the owner
unresolved. A dead receipt survives removal of the matching lock file. This
covers newly journaled readers after process exit on the same intact local
volume; it provides no death proof for copied registries or lost volumes.

Recovery examines 32 obligations per pass, advances past errors, and prunes empty
dead owners separately. Local retry, recovery, owner pruning, and root retirement
have independent time budgets. Admission stops at 4,096 outstanding readers or
256 registered incarnations. Existing active readers keep their protection when
these limits are reached. The [local verification record](verification/chunk-readers-2026-09-13/README.md) includes Go race results and Linux subprocess/HTTP tests.

The collector operates on one retired prefix, uses conditional deletion, handles
replacement races, and preserves a permanent catalog tombstone. Its counters
include only confirmed deletions. Repeated cleanup under a deleted catalog may
remove late empty placeholders; unexpected nonempty data is an error. No server
loop invokes collection, and legacy global chunks remain outside this protocol.

## Implementation gates

- Verify the integrated handoff path on native Linux and the provider, including
  a real VM with lazy faults during root retirement. Local tests use the real
  client and server paths against controlled HTTP storage and peer fixtures.
- Reconcile legacy readers that predate the local obligation journal, unfinished
  publishers lacking both valid retained source and complete cloud receipts, and
  worker-volume loss. Local process locks cover newly journaled readers and
  publisher attempts. Cloud receipts recover completed backups after source
  loss with the registry intact. Never guess death from age or missing heartbeats.
- Handle every durable root, including stable hibernation keys, local markers,
  and saved-snapshot metadata. The implemented generation handoff path is only
  one part of this migration. Root retirement now resumes from the local registry;
  losing that registry or an entire worker volume still needs reconciliation
  against the cloud catalog and authority records.
- Resolve aborted and ambiguous writes before enabling collection. Never infer
  completion merely because a client context was cancelled.
- Isolate the new format from legacy producers. Existing global chunks and the
  indefinite `chunksUploaded` cache cannot participate in a new collector.
- Measure catalog contention and encoded size under actual fork fanout. Holder
  limits must reject admission explicitly and preserve existing holders.
- Verify bucket versioning and soft-deletion settings. Measure current payload,
  historical versions, tombstones, and unresolved obligations separately.
- Prove repeated reclaim and restore cycles against actual storage, including
  active lazy readers and failed publication. Keep saved-snapshot tombstones and
  ownership fences intact.

The [runnable models](../scripts/storage-models/README.md) pass 65,488 cloud-ready
states and 412,233 pending-backup states. They include deliberate failing variants
to check that the assertions detect lost protection and late writes. These are
bounded protocol checks. They do not establish that the integration gates above
have been satisfied.

The VM callback passed the full Go suite, VM race tests, and native Linux
ownership tests using temporary sockets and pipes. The background-drain test
uses Go's `testing/synctest`; deliberately moving the callback before the drain
made both prefetch and prewarm checks fail. Production ordering was restored
and the tests passed again. These checks establish the VM callback boundary,
not a native execution of the newly integrated server handoff format.
