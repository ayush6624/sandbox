package chunkstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

const testSet = "11111111-1111-4111-8111-111111111111"
const testOwner = "22222222-2222-4222-8222-222222222222"

type blobObject struct {
	data []byte
	gen  int64
}

type memoryBlob struct {
	mu       sync.Mutex
	objects  map[string]blobObject
	nextGen  int64
	puts     int
	afterPut func() error
}

func newMemoryBlob() *memoryBlob {
	return &memoryBlob{objects: make(map[string]blobObject), nextGen: 1}
}

func (b *memoryBlob) GetBytesGen(ctx context.Context, key string) ([]byte, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[key]
	if !ok {
		return nil, 0, gcsblob.ErrNotExist
	}
	return bytes.Clone(o.data), o.gen, nil
}

func (b *memoryBlob) PutBytesIfGenerationMatch(ctx context.Context, key string, data []byte, want int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.puts++
	if b.objects[key].gen != want {
		b.mu.Unlock()
		return 0, gcsblob.ErrPreconditionFailed
	}
	gen := b.nextGen
	b.nextGen++
	b.objects[key] = blobObject{data: bytes.Clone(data), gen: gen}
	hook := b.afterPut
	b.afterPut = nil
	b.mu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return 0, err
		}
	}
	return gen, nil
}

func (b *memoryBlob) seed(t *testing.T, c *catalog) []byte {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.objects[catalogKey(testSet)] = blobObject{data: bytes.Clone(data), gen: b.nextGen}
	b.nextGen++
	b.mu.Unlock()
	return data
}

func fixtureCatalog() *catalog {
	return &catalog{Version: 1, SetID: testSet, Phase: Live,
		Publisher: publisher{ID: "publisher"}, Roots: map[string]root{"root": {}}, Readers: map[string]reader{}}
}

func newLiveStore(t *testing.T) (*Store, *memoryBlob) {
	t.Helper()
	blob := newMemoryBlob()
	store := New(blob)
	must(t, store.Begin(context.Background(), testSet, "publisher"))
	must(t, store.AttachRoot(context.Background(), testSet, "publisher", "root"))
	return store, blob
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func inspect(t *testing.T, store *Store) View {
	t.Helper()
	v, err := store.Inspect(context.Background(), testSet)
	must(t, err)
	return v
}

func TestLifecyclePreservesReaderAndPublisher(t *testing.T) {
	ctx := context.Background()
	store, _ := newLiveStore(t)
	r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
	must(t, err)
	must(t, store.RetireRoot(ctx, testSet, "root"))
	v := inspect(t, store)
	if v.Phase != Live || v.Publisher.Released || len(v.Readers) != 1 || !v.Roots[0].Retired {
		t.Fatalf("retirement dropped independent protection: %+v", v)
	}
	if err := store.MarkDeleting(ctx, testSet); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("collection with holders: %v", err)
	}
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	if inspect(t, store).Phase != Live {
		t.Fatal("publisher release retired an active reader")
	}
	must(t, r.Close(ctx))
	must(t, r.Close(ctx))
	if v := inspect(t, store); v.Phase != Retired || len(v.Readers) != 0 {
		t.Fatalf("last release did not retire atomically: %+v", v)
	}
	must(t, store.MarkDeleting(ctx, testSet))
	must(t, store.MarkDeleting(ctx, testSet))
	must(t, store.MarkDeleted(ctx, testSet))
	must(t, store.MarkDeleted(ctx, testSet))
	must(t, store.MarkDeleting(ctx, testSet))
	must(t, store.RetireRoot(ctx, testSet, "root"))
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	if v := inspect(t, store); v.Phase != Deleted {
		t.Fatalf("replay changed tombstone: %+v", v)
	}
	if err := store.Begin(ctx, testSet, "publisher"); !errors.Is(err, ErrRetired) {
		t.Fatalf("Begin resurrected deleted set: %v", err)
	}
}

func TestPublicationAndIdentityReplays(t *testing.T) {
	ctx := context.Background()
	store, blob := newLiveStore(t)
	before := blob.puts
	must(t, store.Begin(ctx, testSet, "publisher"))
	must(t, store.AttachRoot(ctx, testSet, "publisher", "root"))
	if blob.puts != before {
		t.Fatal("idempotent replay wrote a catalog")
	}
	for name, action := range map[string]func() error{
		"new publisher":             func() error { return store.Begin(ctx, testSet, "other") },
		"publisher as root":         func() error { return store.AttachRoot(ctx, testSet, "publisher", "publisher") },
		"root as publisher":         func() error { return store.ReleasePublisher(ctx, testSet, "root") },
		"publisher as retired root": func() error { return store.RetireRoot(ctx, testSet, "publisher") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := action(); !errors.Is(err, ErrIdentityConflict) {
				t.Fatalf("expected identity conflict: %v", err)
			}
		})
	}
	must(t, store.RetireRoot(ctx, testSet, "root"))
	if err := store.AttachRoot(ctx, testSet, "publisher", "root"); !errors.Is(err, ErrReleased) {
		t.Fatalf("root receipt allowed resurrection: %v", err)
	}
	must(t, store.AttachRoot(ctx, testSet, "publisher", "other-root"))
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	if err := store.AttachRoot(ctx, testSet, "publisher", "third-root"); !errors.Is(err, ErrReleased) {
		t.Fatalf("released publisher could attach: %v", err)
	}
}

func TestLostAcquireResponseAfterOtherReaderAndRetirement(t *testing.T) {
	ctx := context.Background()
	store, blob := newLiveStore(t)
	var other *Reader
	blob.afterPut = func() error {
		var err error
		other, err = acquireTestReader(ctx, store, testSet, "root", uuid.NewString())
		must(t, err)
		must(t, store.RetireRoot(ctx, testSet, "root"))
		return io.ErrUnexpectedEOF
	}
	first, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
	must(t, err)
	v := inspect(t, store)
	if len(v.Readers) != 2 || !v.Roots[0].Retired {
		t.Fatalf("lost response clobbered intervening changes: %+v", v)
	}
	must(t, first.Close(ctx))
	must(t, other.Close(ctx))
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	if inspect(t, store).Phase != Retired {
		t.Fatal("reader acquisition left an extra holder")
	}
}

func TestLostReleaseResponsePreservesNewReader(t *testing.T) {
	ctx := context.Background()
	store, blob := newLiveStore(t)
	first, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
	must(t, err)
	var other *Reader
	blob.afterPut = func() error {
		var err error
		other, err = acquireTestReader(ctx, store, testSet, "root", testOwner)
		must(t, err)
		return io.ErrUnexpectedEOF
	}
	if err := first.Close(ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost close response must remain unresolved: %v", err)
	}
	must(t, first.Close(ctx))
	v := inspect(t, store)
	if len(v.Readers) != 1 || v.Readers[0].ID != other.Identity().ReaderID {
		t.Fatalf("close replay lost another holder: %+v", v)
	}
	must(t, other.Close(ctx))
}

type initialReadBarrier struct {
	BlobStore
	mu        sync.Mutex
	remaining int
	ready     chan struct{}
}

func (b *initialReadBarrier) GetBytesGen(ctx context.Context, key string) ([]byte, int64, error) {
	data, gen, err := b.BlobStore.GetBytesGen(ctx, key)
	b.mu.Lock()
	wait := b.remaining > 0
	if wait {
		b.remaining--
		if b.remaining == 0 {
			close(b.ready)
		}
	}
	b.mu.Unlock()
	if wait {
		<-b.ready
	}
	return data, gen, err
}

func TestReadersRaceRootRetirement(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		ctx := context.Background()
		store, blob := newLiveStore(t)
		must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
		store = New(&initialReadBarrier{BlobStore: blob, remaining: 3, ready: make(chan struct{})})
		type result struct {
			reader *Reader
			err    error
		}
		results := make(chan result, 2)
		retired := make(chan error, 1)
		for i := 0; i < 2; i++ {
			go func() {
				r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
				results <- result{r, err}
			}()
		}
		go func() { retired <- store.RetireRoot(ctx, testSet, "root") }()
		must(t, <-retired)
		var handles []*Reader
		for i := 0; i < 2; i++ {
			r := <-results
			if r.err != nil && !errors.Is(r.err, ErrReleased) && !errors.Is(r.err, ErrRetired) {
				t.Fatal(r.err)
			}
			if r.reader != nil {
				handles = append(handles, r.reader)
			}
		}
		v := inspect(t, store)
		if len(v.Readers) != len(handles) {
			t.Fatalf("successful readers lost protection: %+v, handles=%d", v, len(handles))
		}
		for _, r := range handles {
			must(t, r.Close(ctx))
		}
		if inspect(t, store).Phase != Retired {
			t.Fatal("last holder did not retire")
		}
	}
}

func TestReaderIdentitiesDoNotAccumulate(t *testing.T) {
	ctx := context.Background()
	store, _ := newLiveStore(t)
	for i := 0; i < MaxActiveHolders+1; i++ {
		r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
		must(t, err)
		must(t, r.Close(ctx))
	}
	if v := inspect(t, store); len(v.Readers) != 0 || len(v.Roots) != 1 {
		t.Fatalf("reader receipts accumulated: %+v", v)
	}
}

func TestActiveHolderCapacityPreservesCatalog(t *testing.T) {
	ctx := context.Background()
	blob := newMemoryBlob()
	c := fixtureCatalog()
	for i := 0; i < MaxActiveHolders-2; i++ {
		c.Readers[uuid.NewString()] = reader{RootID: "root", OwnerID: testOwner}
	}
	before := blob.seed(t, c)
	store := New(blob)
	if _, err := acquireTestReader(ctx, store, testSet, "root", testOwner); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected active capacity rejection: %v", err)
	}
	after, _, err := blob.GetBytesGen(ctx, catalogKey(testSet))
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("capacity rejection changed holders")
	}
	for id := range c.Readers {
		recovered, err := store.RecoverReader(ReaderIdentity{SetID: testSet, RootID: "root", OwnerID: testOwner, ReaderID: id})
		must(t, err)
		must(t, recovered.Close(ctx))
		break
	}
	r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
	must(t, err)
	must(t, r.Close(ctx))
}

func TestEncodedCapacityPreservesRootReceipts(t *testing.T) {
	ctx := context.Background()
	blob := newMemoryBlob()
	c := fixtureCatalog()
	base, err := json.Marshal(c)
	must(t, err)
	id := strings.Repeat("x", 240) + fmt.Sprintf("%016d", 0)
	c.Roots[id] = root{Retired: true}
	one, err := json.Marshal(c)
	must(t, err)
	growth := len(one) - len(base)
	count := (MaxCatalogBytes - len(base)) / growth
	for i := 0; i < count; i++ {
		c.Roots[strings.Repeat("x", 240)+fmt.Sprintf("%016d", i)] = root{Retired: true}
	}
	before := blob.seed(t, c)
	store := New(blob)
	if _, err := store.Inspect(ctx, testSet); err != nil {
		t.Fatalf("fixture is invalid: %v, %d bytes", err, len(before))
	}
	newRoot := strings.Repeat("y", 256)
	if err := store.AttachRoot(ctx, testSet, "publisher", newRoot); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected encoded capacity rejection: %v, %d bytes", err, len(before))
	}
	after, _, err := blob.GetBytesGen(ctx, catalogKey(testSet))
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("encoded capacity rejection changed catalog")
	}
}

func TestInvalidCatalogsFailClosed(t *testing.T) {
	good, err := json.Marshal(fixtureCatalog())
	must(t, err)
	cases := map[string][]byte{
		"malformed":         []byte("{"),
		"unknown field":     bytes.Replace(good, []byte(`"version":1`), []byte(`"future":true,"version":1`), 1),
		"duplicate field":   bytes.Replace(good, []byte(`"readers":{}`), []byte(`"readers":{},"readers":{}`), 1),
		"trailing document": append(bytes.Clone(good), []byte(` {}`)...),
		"oversize":          bytes.Repeat([]byte(" "), MaxCatalogBytes+1),
	}
	for name, mutate := range map[string]func(*catalog){
		"wrong version":           func(c *catalog) { c.Version = 2 },
		"wrong set":               func(c *catalog) { c.SetID = uuid.NewString() },
		"unknown phase":           func(c *catalog) { c.Phase = "future" },
		"nil holder map":          func(c *catalog) { c.Readers = nil },
		"retired with holders":    func(c *catalog) { c.Phase = Retired },
		"empty live":              func(c *catalog) { c.Publisher.Released = true; c.Roots["root"] = root{Retired: true} },
		"publishing with root":    func(c *catalog) { c.Phase = Publishing },
		"identity kind collision": func(c *catalog) { c.Roots["publisher"] = root{} },
		"reader unknown root":     func(c *catalog) { c.Readers[uuid.NewString()] = reader{RootID: "missing", OwnerID: testOwner} },
		"reader invalid owner":    func(c *catalog) { c.Readers[uuid.NewString()] = reader{RootID: "root", OwnerID: "bad"} },
	} {
		c := fixtureCatalog()
		mutate(c)
		cases[name], err = json.Marshal(c)
		must(t, err)
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			blob := newMemoryBlob()
			blob.objects[catalogKey(testSet)] = blobObject{data: data, gen: 1}
			store := New(blob)
			if err := store.MarkDeleting(context.Background(), testSet); !errors.Is(err, ErrInvalidCatalog) {
				t.Fatalf("invalid catalog accepted: %v", err)
			}
			if blob.puts != 0 {
				t.Fatal("invalid catalog was mutated")
			}
		})
	}
}

func TestInvalidIdentitiesAndReadGeneration(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"", "../catalog", "1234", uuid.Nil.String(), strings.ToUpper("a1111111-1111-4111-8111-111111111111")} {
		if err := New(newMemoryBlob()).Begin(ctx, id, "publisher"); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("set ID %q accepted: %v", id, err)
		}
	}
	store, blob := newLiveStore(t)
	for _, id := range []string{"", " publisher", "publisher\n", strings.Repeat("x", 257)} {
		if err := store.AttachRoot(ctx, testSet, "publisher", id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("root ID %q accepted: %v", id, err)
		}
	}
	if _, err := acquireTestReader(ctx, store, testSet, "root", "not-a-process-uuid"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid owner accepted: %v", err)
	}
	blob.mu.Lock()
	o := blob.objects[catalogKey(testSet)]
	o.gen = 0
	blob.objects[catalogKey(testSet)] = o
	blob.mu.Unlock()
	if _, err := store.Inspect(ctx, testSet); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("missing numeric generation accepted: %v", err)
	}
}

func TestConcurrentReaderCloseAndCancelledRetry(t *testing.T) {
	ctx := context.Background()
	store, _ := newLiveStore(t)
	r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
	must(t, err)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close: %v", err)
	}
	if len(inspect(t, store).Readers) != 1 {
		t.Fatal("cancelled close lost holder")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- r.Close(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if len(inspect(t, store).Readers) != 0 {
		t.Fatal("concurrent closes left a holder")
	}
}

func TestLostBeginResponsePreservesInterveningRoot(t *testing.T) {
	ctx := context.Background()
	blob := newMemoryBlob()
	store := New(blob)
	blob.afterPut = func() error {
		must(t, store.AttachRoot(ctx, testSet, "publisher", "root"))
		return io.ErrUnexpectedEOF
	}
	must(t, store.Begin(ctx, testSet, "publisher"))
	if v := inspect(t, store); v.Phase != Live || len(v.Roots) != 1 {
		t.Fatalf("Begin replay replaced the later root attachment: %+v", v)
	}
}

type failedWrites struct {
	BlobStore
	calls int
}

func (b *failedWrites) PutBytesIfGenerationMatch(context.Context, string, []byte, int64) (int64, error) {
	b.calls++
	return 0, io.ErrUnexpectedEOF
}

func TestUnobservedAmbiguousWriteReturnsErrorWithoutBlindRetry(t *testing.T) {
	blob := &failedWrites{BlobStore: newMemoryBlob()}
	err := New(blob).Begin(context.Background(), testSet, "publisher")
	if !errors.Is(err, io.ErrUnexpectedEOF) || blob.calls != 1 {
		t.Fatalf("unconfirmed write was retried: calls=%d, error=%v", blob.calls, err)
	}
}

type delayedCatalogPut struct {
	BlobStore
	captured chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (b *delayedCatalogPut) PutBytesIfGenerationMatch(ctx context.Context, key string, data []byte, generation int64) (int64, error) {
	b.once.Do(func() {
		close(b.captured)
		<-b.resume
	})
	return b.BlobStore.PutBytesIfGenerationMatch(ctx, key, data, generation)
}

func TestAbsentRootRetirementFencesCapturedAttachment(t *testing.T) {
	ctx := context.Background()
	blob := newMemoryBlob()
	store := New(blob)
	must(t, store.Begin(ctx, testSet, "publisher"))
	delayed := &delayedCatalogPut{BlobStore: blob, captured: make(chan struct{}), resume: make(chan struct{})}
	attached := make(chan error, 1)
	go func() { attached <- New(delayed).AttachRoot(ctx, testSet, "publisher", "root") }()
	<-delayed.captured
	err := store.RetireRoot(ctx, testSet, "root")
	close(delayed.resume)
	attachErr := <-attached
	must(t, err)
	if !errors.Is(attachErr, ErrReleased) {
		t.Fatalf("captured attachment resurrected retired root: %v", attachErr)
	}
	v := inspect(t, store)
	if v.Phase != Live || v.Publisher.Released || len(v.Roots) != 1 || !v.Roots[0].Retired {
		t.Fatalf("absent-root receipt lost publisher or root history: %+v", v)
	}
	if blob.puts != 3 {
		t.Fatalf("expected begin, retirement, and rejected captured CAS; got %d puts", blob.puts)
	}
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	if v := inspect(t, store); v.Phase != Retired {
		t.Fatalf("late root pinned released publisher: %+v", v)
	}
}

func TestAbsentRootRetirementPreservesTerminalPhase(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []Phase{Retired, Deleting, Deleted} {
		t.Run(string(phase), func(t *testing.T) {
			blob := newMemoryBlob()
			store := New(blob)
			must(t, store.Begin(ctx, testSet, "publisher"))
			must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
			if phase == Deleting || phase == Deleted {
				must(t, store.MarkDeleting(ctx, testSet))
			}
			if phase == Deleted {
				must(t, store.MarkDeleted(ctx, testSet))
			}
			puts := blob.puts
			must(t, store.RetireRoot(ctx, testSet, "never-attached"))
			v := inspect(t, store)
			if v.Phase != phase || len(v.Roots) != 0 || blob.puts != puts {
				t.Fatalf("absent-root replay changed terminal catalog: %+v, puts %d -> %d", v, puts, blob.puts)
			}
			if err := store.AttachRoot(ctx, testSet, "publisher", "never-attached"); !errors.Is(err, ErrRetired) {
				t.Fatalf("terminal phase admitted attachment: %v", err)
			}
		})
	}
	if err := New(newMemoryBlob()).RetireRoot(ctx, testSet, "root"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing catalog must remain unresolved: %v", err)
	}
}

func TestAbsentRootRetirementLostResponsePreservesUnrelatedAttachment(t *testing.T) {
	ctx := context.Background()
	blob := newMemoryBlob()
	store := New(blob)
	must(t, store.Begin(ctx, testSet, "publisher"))
	blob.afterPut = func() error {
		must(t, store.AttachRoot(ctx, testSet, "publisher", "other"))
		return io.ErrUnexpectedEOF
	}
	must(t, store.RetireRoot(ctx, testSet, "root"))
	v := inspect(t, store)
	if v.Phase != Live || len(v.Roots) != 2 || v.Roots[0].ID != "other" || v.Roots[0].Retired || v.Roots[1].ID != "root" || !v.Roots[1].Retired {
		t.Fatalf("retirement replay clobbered unrelated attachment: %+v", v)
	}
}
