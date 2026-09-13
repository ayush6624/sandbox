# Storage lifecycle models

These bounded state explorations test the proposed remote storage protocol.
They do not invoke production code or access cloud storage. Run with Python 3.10
or newer, without `-O`, because assertions check the results:

```sh
python3 scripts/storage-models/model.py
python3 scripts/storage-models/peer_pending.py
python3 scripts/storage-models/write_fence.py
```

The first command explores a generation with cloud data already available.
The second publishes a peer-backed generation while its backup is still pending.
The third checks conditional payload writes that can finish after producer death.
All require the correct protocol to pass and deliberately unsafe variants to
produce counterexamples. A printed `COUNTEREXAMPLE` is expected; a nonzero exit
means a required check failed.

September 13, 2026 results:

| Model | Reachable states | Fully reclaimed terminal states |
| --- | ---: | ---: |
| Cloud-ready generation | 65,488 | 9 |
| Pending peer backup | 412,233 | 9 |

The write-fence model passes 918 states, including 135 states after completed
collection. Thirty contain a late zero-byte placeholder, and none contains
payload bytes. Its bound is one object, one producer, one root, one collector,
two empty-create requests and two payload requests. Dispatch and provider commit
are separate transitions, including commits after producer death. Unsafe variants
demonstrate why payload writes need a positive generation, empty placeholders
must be collected, and a stale DELETE must trigger another read.

Each exploration has one generation, one publisher, one durable root, two
readers, and four holder slots. The peer extension adds one backup worker and
one retained peer source. Catalog reads and conditional writes are separate
steps. The collector can restart during deletion. Writes may commit while their
responses are lost.

The checks cover a root being retired while readers or backup uploads remain,
two readers acquiring concurrently, stale catalog writes, and switching from peer
to cloud reads. Unsafe variants demonstrate overwritten reader protection,
premature publisher release, removal of the only readable copy, and a late PUT
leaving payload bytes after collection.

The first two models assume a resolved upload cannot commit again. Production must account
for every request and retry. Reading matching bytes or proving process death does
not establish that another accepted PUT cannot finish later. Unknown upload
outcomes retain the publisher obligation unless a storage-enforced fence prevents
late writes.

The third model checks one such fence. First create a zero-byte placeholder,
then condition every payload attempt on that placeholder's exact generation.
Deletion or replacement invalidates those requests. A delayed create-only request
can restore an empty object, but a dead producer cannot learn its new generation
and authorize another payload write. A live producer remains protected until it
stops issuing requests and drains them. Empty-object metadata residue still needs
a later sweep. This model assumes authoritative death evidence and correct
provider precondition enforcement; it does not prove either.

The opt-in transport test uses the production GCS client and media-upload API:

```sh
SANDBOX_GCS_TEST_BUCKET=YOUR_BUCKET go test ./internal/gcsblob -run '^TestGCSPlaceholderWriteFence$' -count=1 -v
```

Run it on GCE with an attached identity allowed to create and delete objects in
that bucket. It writes only a fresh UUID key under `_verification/storage-write-fence/`,
checks stale PUT and DELETE rejection, and removes that key. It does not simulate
an in-flight upload or process death. Resumable and streaming finalization still
need separate verification before applying the fence to those transports.

The models abstract the payload set into a single data object and one manifest.
They omit partial upload abort, process recovery, peer-server death, multiple
durable roots, mixed releases, migration, provider versioning, and actual source
draining. They establish safety only within the modeled states and do not prove
eventual cleanup under arbitrary failures. See the
[remote storage design](../../docs/chunk-storage-design.md) for integration work.
