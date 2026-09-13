package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
)

type payloadTestBlob struct {
	*memoryBlob
	mu2              sync.Mutex
	pageSize         int
	emptyFirstPage   bool
	repeatPageToken  bool
	blockUpload      chan struct{}
	uploadBlocked    chan struct{}
	blockOnce        sync.Once
	beforeDelete     func(string, int64)
	beforeDeleteOnce sync.Once
	deleteCalls      int
}

func newPayloadTestBlob() *payloadTestBlob {
	return &payloadTestBlob{memoryBlob: newMemoryBlob()}
}

func (b *payloadTestBlob) PutBytesIfGenerationMatch(ctx context.Context, key string, data []byte, want int64) (int64, error) {
	if len(data) > 0 && b.blockUpload != nil {
		b.blockOnce.Do(func() { close(b.uploadBlocked) })
		select {
		case <-b.blockUpload:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return b.memoryBlob.PutBytesIfGenerationMatch(ctx, key, data, want)
}

func (b *payloadTestBlob) PutSparseIfGenerationMatch(ctx context.Context, key, path string, want int64) (int64, error) {
	var encoded bytes.Buffer
	if _, err := gcsblob.WriteSparse(&encoded, path); err != nil {
		return 0, err
	}
	return b.PutBytesIfGenerationMatch(ctx, key, encoded.Bytes(), want)
}

func (b *payloadTestBlob) PutRangesIfGenerationMatch(ctx context.Context, key, path string, ranges []gcsblob.Range, want int64) (int64, error) {
	var encoded bytes.Buffer
	if _, err := gcsblob.WriteRanges(&encoded, path, ranges); err != nil {
		return 0, err
	}
	return b.PutBytesIfGenerationMatch(ctx, key, encoded.Bytes(), want)
}

func (b *payloadTestBlob) Stat(ctx context.Context, key string) (gcsblob.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return gcsblob.ObjectInfo{}, err
	}
	b.memoryBlob.mu.Lock()
	defer b.memoryBlob.mu.Unlock()
	o, ok := b.objects[key]
	if !ok {
		return gcsblob.ObjectInfo{}, gcsblob.ErrNotExist
	}
	return gcsblob.ObjectInfo{Name: key, Generation: o.gen, Size: int64(len(o.data))}, nil
}

func (b *payloadTestBlob) DownloadIfGenerationMatch(ctx context.Context, key string, want int64, dst io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b.memoryBlob.mu.Lock()
	o, ok := b.objects[key]
	if !ok {
		b.memoryBlob.mu.Unlock()
		return 0, gcsblob.ErrNotExist
	}
	if o.gen != want {
		b.memoryBlob.mu.Unlock()
		return 0, gcsblob.ErrPreconditionFailed
	}
	data := bytes.Clone(o.data)
	b.memoryBlob.mu.Unlock()
	n, err := dst.Write(data)
	return int64(n), err
}

func (b *payloadTestBlob) ListObjects(ctx context.Context, prefix, token string) (gcsblob.ObjectPage, error) {
	if err := ctx.Err(); err != nil {
		return gcsblob.ObjectPage{}, err
	}
	b.mu2.Lock()
	if b.emptyFirstPage && token == "" {
		b.emptyFirstPage = false
		b.mu2.Unlock()
		return gcsblob.ObjectPage{NextPageToken: "after-empty"}, nil
	}
	b.mu2.Unlock()
	b.memoryBlob.mu.Lock()
	var names []string
	for name := range b.objects {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	start := 0
	if token != "" && token != "after-empty" {
		for start < len(names) && names[start] <= token {
			start++
		}
	}
	limit := len(names)
	if b.pageSize > 0 && start+b.pageSize < limit {
		limit = start + b.pageSize
	}
	page := gcsblob.ObjectPage{}
	for _, name := range names[start:limit] {
		o := b.objects[name]
		page.Objects = append(page.Objects, gcsblob.ObjectInfo{Name: name, Generation: o.gen, Size: int64(len(o.data))})
	}
	if limit < len(names) {
		page.NextPageToken = names[limit-1]
	}
	b.memoryBlob.mu.Unlock()
	b.mu2.Lock()
	if b.repeatPageToken {
		if token == "" && page.NextPageToken != "" {
			page.NextPageToken = "repeat"
		} else if token != "" {
			page.NextPageToken = token
		}
	}
	b.mu2.Unlock()
	return page, nil
}

func (b *payloadTestBlob) DeleteIfGenerationMatch(ctx context.Context, key string, want int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	b.mu2.Lock()
	b.deleteCalls++
	hook := b.beforeDelete
	b.mu2.Unlock()
	if hook != nil {
		b.beforeDeleteOnce.Do(func() { hook(key, want) })
	}
	b.memoryBlob.mu.Lock()
	defer b.memoryBlob.mu.Unlock()
	o, ok := b.objects[key]
	if !ok {
		return false, nil
	}
	if o.gen != want {
		return false, gcsblob.ErrPreconditionFailed
	}
	delete(b.objects, key)
	return true, nil
}

func (b *payloadTestBlob) replace(key string, data []byte) {
	b.memoryBlob.mu.Lock()
	b.objects[key] = blobObject{data: bytes.Clone(data), gen: b.nextGen}
	b.nextGen++
	b.memoryBlob.mu.Unlock()
}

func payloadFixture() (*Payloads, *Store, *payloadTestBlob) {
	b := newPayloadTestBlob()
	store := New(b)
	return NewPayloads(store, b), store, b
}

func TestDataPrefix(t *testing.T) {
	if got, want := DataPrefix(testSet), "chunksets/data/"+testSet+"/"; got != want {
		t.Fatalf("DataPrefix=%q, want %q", got, want)
	}
}

func TestPayloadWriterReconcilesLostPlaceholderResponse(t *testing.T) {
	ctx := context.Background()
	payloads, _, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	blob.afterPut = func() error { return io.ErrUnexpectedEOF }
	_, err = w.PutBytes(ctx, "manifest.json", []byte("manifest"))
	must(t, err)
	must(t, w.Close(ctx))
}

func TestPayloadWriterIdempotentBytesSparseAndRanges(t *testing.T) {
	ctx := context.Background()
	payloads, _, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = w.PutBytes(ctx, "manifest.json", []byte("manifest"))
	must(t, err)
	_, err = w.PutBytes(ctx, "manifest.json", []byte("manifest"))
	must(t, err)

	path := filepath.Join(t.TempDir(), "memory")
	f, err := os.Create(path)
	must(t, err)
	must(t, f.Truncate(128<<10))
	_, err = f.WriteAt(bytes.Repeat([]byte{7}, 4096), 64<<10)
	must(t, err)
	must(t, f.Close())
	_, err = w.PutSparse(ctx, "memory.sparse", path)
	must(t, err)
	_, err = w.PutSparse(ctx, "memory.sparse", path)
	must(t, err)
	ranges := []gcsblob.Range{{Off: 64 << 10, Len: 4096}}
	_, err = w.PutRanges(ctx, "memory.diff", path, ranges)
	must(t, err)
	_, err = w.PutRanges(ctx, "memory.diff", path, ranges)
	must(t, err)
	f, err = os.OpenFile(path, os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteAt([]byte{9}, 64<<10)
	must(t, err)
	must(t, f.Close())
	_, err = w.PutSparse(ctx, "memory.sparse", path)
	if !errors.Is(err, ErrImmutablePayload) {
		t.Fatalf("sparse immutable mismatch: %v", err)
	}

	for _, name := range []string{"manifest.json", "memory.sparse", "memory.diff"} {
		if o := blob.objects[payloadKey(testSet, name)]; len(o.data) == 0 || o.gen <= 0 {
			t.Fatalf("%s was not committed: %+v", name, o)
		}
	}
	must(t, w.Close(ctx))
}

func TestPayloadWriterRejectsImmutableMismatchAndInvalidNames(t *testing.T) {
	ctx := context.Background()
	payloads, store, _ := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = w.PutBytes(ctx, "object", []byte("first"))
	must(t, err)
	if _, err := w.PutBytes(ctx, "object", []byte("second")); !errors.Is(err, ErrImmutablePayload) {
		t.Fatalf("immutable mismatch: %v", err)
	}
	if inspect(t, store).Publisher.Released {
		t.Fatal("Put error released publisher")
	}
	for _, name := range []string{"", "/absolute", "../catalog", "a/../catalog", "a//b", "a\n"} {
		if _, err := w.PutBytes(ctx, name, []byte("x")); !errors.Is(err, ErrInvalidPayloadName) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if _, err := w.PutBytes(ctx, "empty", nil); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty payload: %v", err)
	}
	must(t, w.Close(ctx))
	if _, err := w.PutBytes(ctx, "late", []byte("x")); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestWriterCloseWaitsForUploadBeforePublisherRelease(t *testing.T) {
	ctx := context.Background()
	payloads, store, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	blob.blockUpload = make(chan struct{})
	blob.uploadBlocked = make(chan struct{})
	putDone := make(chan error, 1)
	go func() { _, err := w.PutBytes(ctx, "blocked", []byte("payload")); putDone <- err }()
	<-blob.uploadBlocked
	closeDone := make(chan error, 1)
	go func() { closeDone <- w.Close(ctx) }()
	if inspect(t, store).Publisher.Released {
		t.Fatal("publisher released while upload was blocked")
	}
	close(blob.blockUpload)
	must(t, <-putDone)
	must(t, <-closeDone)
	if !inspect(t, store).Publisher.Released {
		t.Fatal("publisher was not released after upload drained")
	}
}

func TestCancelledWriterCloseDrainsButRetainsPublisherForRetry(t *testing.T) {
	ctx := context.Background()
	payloads, store, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	blob.blockUpload = make(chan struct{})
	blob.uploadBlocked = make(chan struct{})
	putDone := make(chan error, 1)
	go func() { _, err := w.PutBytes(ctx, "blocked", []byte("payload")); putDone <- err }()
	<-blob.uploadBlocked
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- w.Close(cancelled) }()
	select {
	case <-closeDone:
		t.Fatal("Close returned before its admitted upload drained")
	default:
	}
	close(blob.blockUpload)
	must(t, <-putDone)
	if err := <-closeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close: %v", err)
	}
	if inspect(t, store).Publisher.Released {
		t.Fatal("cancelled Close released publisher")
	}
	must(t, w.Close(ctx))
	if !inspect(t, store).Publisher.Released {
		t.Fatal("retry did not release publisher")
	}
}

func TestSuspendDrainsRetainsPublisherAndAllowsRetryAttempt(t *testing.T) {
	ctx := context.Background()
	payloads, store, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	blob.blockUpload = make(chan struct{})
	blob.uploadBlocked = make(chan struct{})
	putDone := make(chan error, 1)
	go func() { _, err := w.PutBytes(ctx, "payload", []byte("data")); putDone <- err }()
	<-blob.uploadBlocked
	suspendDone := make(chan error, 1)
	go func() { suspendDone <- w.Suspend(ctx) }()
	select {
	case <-suspendDone:
		t.Fatal("Suspend returned before its admitted upload drained")
	default:
	}
	close(blob.blockUpload)
	must(t, <-putDone)
	must(t, <-suspendDone)
	if inspect(t, store).Publisher.Released {
		t.Fatal("Suspend released publisher")
	}
	if err := w.Close(ctx); !errors.Is(err, ErrWriterSuspended) {
		t.Fatalf("old attempt released publisher: %v", err)
	}
	retry, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = retry.PutBytes(ctx, "payload", []byte("data"))
	must(t, err)
	must(t, retry.Close(ctx))
	if !inspect(t, store).Publisher.Released {
		t.Fatal("successful retry did not release publisher")
	}
}

func TestCollectRequiresRetiredCatalog(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(*Store) func()
	}{
		{"publisher", func(store *Store) func() {
			must(t, store.Begin(ctx, testSet, "publisher"))
			return func() { must(t, store.ReleasePublisher(ctx, testSet, "publisher")) }
		}},
		{"root", func(store *Store) func() {
			must(t, store.Begin(ctx, testSet, "publisher"))
			must(t, store.AttachRoot(ctx, testSet, "publisher", "root"))
			must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
			return func() { must(t, store.RetireRoot(ctx, testSet, "root")) }
		}},
		{"reader", func(store *Store) func() {
			must(t, store.Begin(ctx, testSet, "publisher"))
			must(t, store.AttachRoot(ctx, testSet, "publisher", "root"))
			r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
			must(t, err)
			must(t, store.RetireRoot(ctx, testSet, "root"))
			must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
			return func() { must(t, r.Close(ctx)) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloads, store, _ := payloadFixture()
			release := tc.setup(store)
			if _, err := payloads.Collect(ctx, testSet); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("collector entered with active %s: %v", tc.name, err)
			}
			release()
		})
	}
}

func TestCollectRetriesGenerationRaceAndCountsConfirmedDeletes(t *testing.T) {
	ctx := context.Background()
	payloads, store, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = w.PutBytes(ctx, "a", []byte("old"))
	must(t, err)
	_, err = w.PutBytes(ctx, "b", []byte("bb"))
	must(t, err)
	must(t, w.Close(ctx))
	key := payloadKey(testSet, "a")
	other := payloadKey("33333333-3333-4333-8333-333333333333", "a")
	blob.replace(other, []byte("other"))
	blob.beforeDelete = func(got string, _ int64) {
		if got == key {
			blob.replace(key, []byte("replacement"))
		}
	}
	blob.pageSize = 1
	blob.emptyFirstPage = true
	got, err := payloads.Collect(ctx, testSet)
	must(t, err)
	if got.Objects != 2 || got.Bytes != int64(len("replacement")+len("bb")) {
		t.Fatalf("collection counters: %+v", got)
	}
	if v := inspect(t, store); v.Phase != Deleted {
		t.Fatalf("catalog phase: %s", v.Phase)
	}
	if _, ok := blob.objects[catalogKey(testSet)]; !ok {
		t.Fatal("collector deleted catalog tombstone")
	}
	if _, ok := blob.objects[other]; !ok {
		t.Fatal("collector crossed generation prefix")
	}
}

func TestDeletedCollectionRemovesOnlyLateEmptyResidue(t *testing.T) {
	ctx := context.Background()
	payloads, store, blob := payloadFixture()
	must(t, store.Begin(ctx, testSet, "publisher"))
	must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
	_, err := payloads.Collect(ctx, testSet)
	must(t, err)
	empty := payloadKey(testSet, "late-empty")
	blob.replace(empty, nil)
	got, err := payloads.Collect(ctx, testSet)
	must(t, err)
	if got.Objects != 1 || got.Bytes != 0 {
		t.Fatalf("late empty counters: %+v", got)
	}
	nonempty := payloadKey(testSet, "late-nonempty")
	blob.replace(nonempty, []byte("unexpected"))
	if _, err := payloads.Collect(ctx, testSet); !errors.Is(err, ErrDeletedPayload) {
		t.Fatalf("late nonempty payload: %v", err)
	}
	if _, ok := blob.objects[nonempty]; !ok {
		t.Fatal("collector deleted invariant-violating payload")
	}
}

func TestCollectRejectsRepeatedPageToken(t *testing.T) {
	ctx := context.Background()
	payloads, _, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = w.PutBytes(ctx, "a", []byte("a"))
	must(t, err)
	_, err = w.PutBytes(ctx, "b", []byte("b"))
	must(t, err)
	must(t, w.Close(ctx))
	blob.pageSize = 1
	blob.repeatPageToken = true
	if _, err := payloads.Collect(ctx, testSet); !errors.Is(err, ErrRepeatedPageToken) {
		t.Fatalf("repeated token: %v", err)
	}
	if inspect(t, New(blob)).Phase != Deleting {
		t.Fatal("uncertain listing removed tombstone or completed collection")
	}
}
