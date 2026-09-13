package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/gcsblob"
)

func readerJournalSQL(t *testing.T, s *Server, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", s.reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func assertReaderJournal(t *testing.T, s *Server, count int) {
	t.Helper()
	rows, err := s.reg.ListChunkReaders(context.Background(), "", 1000)
	if err != nil || len(rows) != count {
		t.Fatalf("reader journal: %d, want %d: %v", len(rows), count, err)
	}
}

func TestChunkReaderJournalPrecedesAcquisition(t *testing.T) {
	s, store, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	key := "chunksets/catalog/" + d.Ref.Generation + ".json"
	before := store.putCount(key)
	readerJournalSQL(t, s, `CREATE TRIGGER reject_reader BEFORE INSERT ON chunk_readers BEGIN SELECT RAISE(ABORT,'journal unavailable'); END`)
	closeReader, err := s.acquireChunkReader(ctx, &d.Manifest)
	if err == nil || closeReader != nil {
		t.Fatal("journal failure admitted reader")
	}
	if got := store.putCount(key); got != before {
		t.Fatalf("catalog changed before journal commit: %d -> %d", before, got)
	}
	assertReaderJournal(t, s, 0)
	readerJournalSQL(t, s, `DROP TRIGGER reject_reader`)
	s.retryChunkReleases(ctx)
	if got := store.putCount(key); got != before {
		t.Fatal("local-only cleanup wrote remote catalog")
	}
	s.chunkOwnership.mu.Lock()
	pending := len(s.chunkOwnership.readers)
	s.chunkOwnership.mu.Unlock()
	if pending != 0 {
		t.Fatalf("local cleanup retained %d handles", pending)
	}
	store.hook(func(_ *http.Request, name string) int {
		if name == key {
			assertReaderJournal(t, s, 1)
		}
		return 0
	})
	closeReader, err = s.acquireChunkReader(ctx, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	store.hook(nil)
	if err := closeReader(); err != nil {
		t.Fatal(err)
	}
	assertReaderJournal(t, s, 0)
}

func TestChunkReaderRecoveryProtectsActiveUntilDrain(t *testing.T) {
	s, _, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	closeReader, err := s.acquireChunkReader(ctx, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.destroyHandoff(ctx, d.Record.ID); err != nil {
		t.Fatal(err)
	}
	s.retryChunkReleases(ctx)
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || len(view.Readers) != 1 {
		t.Fatalf("live reader released: %+v, %v", view, err)
	}
	assertReaderJournal(t, s, 1)
	if err := closeReader(); err != nil {
		t.Fatal(err)
	}
	if err := closeReader(); err != nil {
		t.Fatal(err)
	}
	assertReaderJournal(t, s, 0)
	view, err = catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || len(view.Readers) != 0 {
		t.Fatalf("drained reader retained: %+v, %v", view, err)
	}
}

func TestChunkReaderReleaseRetriesLocalCompletion(t *testing.T) {
	s, store, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	closeReader, err := s.acquireChunkReader(ctx, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	readerJournalSQL(t, s, `CREATE TRIGGER reject_reader_delete BEFORE DELETE ON chunk_readers BEGIN SELECT RAISE(ABORT,'local completion unavailable'); END`)
	if err := closeReader(); err == nil {
		t.Fatal("lost local completion reported success")
	}
	assertReaderJournal(t, s, 1)
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || len(view.Readers) != 0 {
		t.Fatalf("remote close failed: %+v, %v", view, err)
	}
	key := "chunksets/catalog/" + d.Ref.Generation + ".json"
	before := store.putCount(key)
	readerJournalSQL(t, s, `DROP TRIGGER reject_reader_delete`)
	s.retryChunkReleases(ctx)
	assertReaderJournal(t, s, 0)
	if got := store.putCount(key); got != before {
		t.Fatalf("closed handle repeated remote write: %d -> %d", before, got)
	}
}

func TestChunkReaderLostAcquireResponseRetainsIdentity(t *testing.T) {
	s, store, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	acquireCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	key := "chunksets/catalog/" + d.Ref.Generation + ".json"
	var intercepted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("name") == key && intercepted.CompareAndSwap(false, true) {
			assertReaderJournal(t, s, 1)
			response := httptest.NewRecorder()
			store.serveHTTP(response, r)
			if response.Code != http.StatusOK {
				t.Errorf("acquisition commit: %d", response.Code)
			}
			cancel()
			return
		}
		store.serveHTTP(w, r)
	}))
	defer srv.Close()
	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.blob = gcsblob.NewWithHTTPClient("upload-test", &http.Client{Transport: uploadTestTransport{target: endpoint, base: http.DefaultTransport}})
	// Publication initialized the catalog client before the response-loss wrapper.
	s.chunkOwnership.catalog = chunkstore.New(s.blob)
	s.chunkOwnership.payloads = chunkstore.NewPayloads(s.chunkOwnership.catalog, s.blob)
	closeReader, err := s.acquireChunkReader(acquireCtx, &d.Manifest)
	if err == nil || closeReader != nil || !intercepted.Load() {
		t.Fatalf("ambiguous acquisition not exercised: %v", err)
	}
	rows, err := s.reg.ListChunkReaders(ctx, "", 32)
	if err != nil || len(rows) != 1 {
		t.Fatalf("acquisition identity lost: %+v, %v", rows, err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || len(view.Readers) != 1 || view.Readers[0].ID != rows[0].ReaderID || view.Readers[0].OwnerID != rows[0].OwnerID {
		t.Fatalf("remote identity differs from journal: %+v, %v", view, err)
	}
	s.retryChunkReleases(ctx)
	assertReaderJournal(t, s, 0)
	view, err = catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || len(view.Readers) != 0 {
		t.Fatalf("ambiguous acquisition leaked: %+v, %v", view, err)
	}
}
