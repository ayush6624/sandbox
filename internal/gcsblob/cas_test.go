package gcsblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCS is a tiny in-memory stand-in for the GCS JSON+media API, implementing
// just enough of the generation/ifGenerationMatch semantics to exercise the CAS
// path: create-only (gen=0), generation-match update, and the 412 loss.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string]fakeObj
	nextGen int64
}

type fakeObj struct {
	data []byte
	gen  int64
}

func newFakeGCS() *fakeGCS {
	return &fakeGCS{objects: map[string]fakeObj{}, nextGen: 1}
}

func (f *fakeGCS) handler() http.Handler {
	mux := http.NewServeMux()
	// Media upload (POST /upload/storage/v1/b/{bucket}/o?name=...&ifGenerationMatch=...)
	mux.HandleFunc("/upload/storage/v1/b/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		payload, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		defer f.mu.Unlock()
		cur, exists := f.objects[name]
		if ig := r.URL.Query().Get("ifGenerationMatch"); ig != "" {
			want, _ := strconv.ParseInt(ig, 10, 64)
			have := int64(0)
			if exists {
				have = cur.gen
			}
			if want != have {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		g := f.nextGen
		f.nextGen++
		f.objects[name] = fakeObj{data: payload, gen: g}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"generation":"%d"}`, g)
	})
	// Media download (GET /storage/v1/b/{bucket}/o/{object}?alt=media)
	mux.HandleFunc("/storage/v1/b/", func(w http.ResponseWriter, r *http.Request) {
		// Object name is the last path segment, URL-escaped by objectURL.
		parts := strings.SplitN(r.URL.Path, "/o/", 2)
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		name := parts[1]
		f.mu.Lock()
		obj, ok := f.objects[name]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.gen, 10))
		w.WriteHeader(200)
		_, _ = w.Write(obj.data)
	})
	return mux
}

// testClient wires a Client at the fake server and seeds a token so no metadata
// server is contacted.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New("test-bucket")
	c.storageBase = srv.URL + "/storage/v1"
	c.uploadBase = srv.URL + "/upload/storage/v1"
	c.tok = "fake-token"
	c.tokExp = time.Now().Add(time.Hour)
	return c
}

// TestCASCreateOnly: gen=0 succeeds on a fresh object and fails (412) once it
// exists — the fence-acquire semantics.
func TestCASCreateOnly(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	c := testClient(t, srv)
	ctx := context.Background()

	g1, err := c.PutBytesIfGenerationMatch(ctx, "hib/x/owner", []byte(`{"host":"A","epoch":1}`), 0)
	if err != nil {
		t.Fatalf("create-only on fresh object: %v", err)
	}
	if g1 <= 0 {
		t.Fatalf("expected positive generation, got %d", g1)
	}
	// A second create-only must lose: the object now exists.
	if _, err := c.PutBytesIfGenerationMatch(ctx, "hib/x/owner", []byte(`{"host":"B","epoch":1}`), 0); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed on second create-only, got %v", err)
	}
	// The fence must still read as A's write.
	b, gen, err := c.GetBytesGen(ctx, "hib/x/owner")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gen != g1 {
		t.Fatalf("generation drifted: read %d, wrote %d", gen, g1)
	}
	if !strings.Contains(string(b), `"host":"A"`) {
		t.Fatalf("fence overwritten by loser: %s", b)
	}
}

// TestCASGenerationMatch: an update with the current generation wins and bumps
// the generation; a stale generation loses — the fence-handoff semantics.
func TestCASGenerationMatch(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	c := testClient(t, srv)
	ctx := context.Background()

	g1, err := c.PutBytesIfGenerationMatch(ctx, "hib/x/owner", []byte(`{"host":"A","epoch":1}`), 0)
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}
	// Host B reads gen g1 and hands ownership to itself with a bumped epoch.
	g2, err := c.PutBytesIfGenerationMatch(ctx, "hib/x/owner", []byte(`{"host":"B","epoch":2}`), g1)
	if err != nil {
		t.Fatalf("generation-match update: %v", err)
	}
	if g2 == g1 {
		t.Fatalf("generation did not advance after update: %d", g2)
	}
	// Host A, still holding the stale g1, must lose its next write.
	if _, err := c.PutBytesIfGenerationMatch(ctx, "hib/x/owner", []byte(`{"host":"A","epoch":3}`), g1); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("expected ErrPreconditionFailed for stale generation, got %v", err)
	}
	b, _, err := c.GetBytesGen(ctx, "hib/x/owner")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(string(b), `"host":"B"`) {
		t.Fatalf("expected B to own the fence, got %s", b)
	}
}

// TestGetBytesGenMissing: an absent object reads as ErrNotExist with gen 0, and
// that 0 is exactly the create-only precondition.
func TestGetBytesGenMissing(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	c := testClient(t, srv)
	ctx := context.Background()

	_, gen, err := c.GetBytesGen(ctx, "hib/missing/owner")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
	if gen != 0 {
		t.Fatalf("expected gen 0 for missing object, got %d", gen)
	}
}
