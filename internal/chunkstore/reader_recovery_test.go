package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

func acquireTestReader(ctx context.Context, store *Store, setID, rootID, ownerID string) (*Reader, error) {
	r, err := store.PrepareReader(setID, rootID, ownerID)
	if err != nil {
		return nil, err
	}
	if err := r.Acquire(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

type capturedReaderPut struct {
	BlobStore
	key        string
	data       []byte
	generation int64
}

func (b *capturedReaderPut) PutBytesIfGenerationMatch(_ context.Context, key string, data []byte, generation int64) (int64, error) {
	b.key, b.data, b.generation = key, bytes.Clone(data), generation
	return 0, io.ErrUnexpectedEOF
}

func TestAbsentReaderCloseFencesCapturedAcquisition(t *testing.T) {
	ctx := context.Background()
	store, blob := newLiveStore(t)
	captured := &capturedReaderPut{BlobStore: blob}
	pending, err := New(captured).PrepareReader(testSet, "root", testOwner)
	must(t, err)
	identity := pending.Identity()
	if err := pending.Acquire(ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ambiguous acquisition: %v", err)
	}
	if pending.Identity() != identity || len(captured.data) == 0 {
		t.Fatal("acquisition lost its reserved identity or never issued its PUT")
	}
	if err := pending.Acquire(ctx); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("ambiguous acquisition allowed a fresh attempt: %v", err)
	}
	if len(inspect(t, store).Readers) != 0 {
		t.Fatal("captured acquisition already committed")
	}
	recovered, err := store.RecoverReader(identity)
	must(t, err)
	must(t, recovered.Close(ctx))
	if _, err := blob.PutBytesIfGenerationMatch(ctx, captured.key, captured.data, captured.generation); !errors.Is(err, gcsblob.ErrPreconditionFailed) {
		t.Fatalf("late acquisition crossed absent-reader close: %v", err)
	}
	if v := inspect(t, store); len(v.Readers) != 0 || v.Phase != Live || v.Publisher.Released || v.Roots[0].Retired {
		t.Fatalf("close changed unrelated protection: %+v", v)
	}
}

func TestFailedAcquisitionRetainsCommittedReaderIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, blob := newLiveStore(t)
	r, err := store.PrepareReader(testSet, "root", testOwner)
	must(t, err)
	identity := r.Identity()
	blob.afterPut = func() error {
		cancel()
		return io.ErrUnexpectedEOF
	}
	if err := r.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled reconciliation: %v", err)
	}
	v := inspect(t, store)
	if r.Identity() != identity || len(v.Readers) != 1 || v.Readers[0].ID != identity.ReaderID || v.Readers[0].OwnerID != identity.OwnerID {
		t.Fatalf("failed acquisition lost its remote identity: %+v, %+v", r.Identity(), v)
	}
	must(t, r.Close(context.Background()))
	if len(inspect(t, store).Readers) != 0 {
		t.Fatal("failed handle could not release committed acquisition")
	}
}

func TestAbsentReaderLostCloseResponseRequiresAnotherCAS(t *testing.T) {
	ctx := context.Background()
	store, blob := newLiveStore(t)
	r, err := store.PrepareReader(testSet, "root", testOwner)
	must(t, err)
	before := blob.puts
	blob.afterPut = func() error { return io.ErrUnexpectedEOF }
	if err := r.Close(ctx); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("absence acknowledged a lost fence response: %v", err)
	}
	if blob.puts != before+1 {
		t.Fatalf("close blindly retried ambiguous write: %d puts", blob.puts-before)
	}
	must(t, r.Close(ctx))
	if blob.puts != before+2 {
		t.Fatalf("close retry did not establish a fresh fence: %d puts", blob.puts-before)
	}
	must(t, r.Close(ctx))
	if blob.puts != before+2 {
		t.Fatal("confirmed close was not idempotent")
	}
}

func TestReaderAcquisitionIsOneShot(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"acquired", "failed", "closed", "failed-close", "recovered"} {
		t.Run(state, func(t *testing.T) {
			store, blob := newLiveStore(t)
			r, err := store.PrepareReader(testSet, "root", testOwner)
			must(t, err)
			switch state {
			case "acquired":
				must(t, r.Acquire(ctx))
			case "failed":
				must(t, store.RetireRoot(ctx, testSet, "root"))
				if err := r.Acquire(ctx); !errors.Is(err, ErrReleased) {
					t.Fatalf("retired root acquisition: %v", err)
				}
			case "closed":
				must(t, r.Close(ctx))
			case "failed-close":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				if err := r.Close(cancelled); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled close: %v", err)
				}
			case "recovered":
				r, err = store.RecoverReader(r.Identity())
				must(t, err)
			}
			before := blob.puts
			if err := r.Acquire(ctx); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("%s reader allowed acquisition: %v", state, err)
			}
			if blob.puts != before {
				t.Fatal("disabled acquisition issued a PUT")
			}
			must(t, r.Close(ctx))
		})
	}
}

func TestReaderRecoveryRejectsConflictingHolder(t *testing.T) {
	ctx := context.Background()
	for _, field := range []string{"owner", "root"} {
		t.Run(field, func(t *testing.T) {
			store, blob := newLiveStore(t)
			r, err := acquireTestReader(ctx, store, testSet, "root", testOwner)
			must(t, err)
			identity := r.Identity()
			if field == "owner" {
				identity.OwnerID = uuid.NewString()
			} else {
				identity.RootID = "other-root"
			}
			recovered, err := store.RecoverReader(identity)
			must(t, err)
			before := blob.puts
			if err := recovered.Close(ctx); !errors.Is(err, ErrIdentityConflict) {
				t.Fatalf("conflicting %s removed reader: %v", field, err)
			}
			if blob.puts != before || len(inspect(t, store).Readers) != 1 {
				t.Fatal("conflict mutated catalog")
			}
			must(t, r.Close(ctx))
		})
	}
}

func TestReaderRecoveryPreservesTerminalPhase(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []Phase{Publishing, Retired, Deleting, Deleted} {
		t.Run(string(phase), func(t *testing.T) {
			blob := newMemoryBlob()
			store := New(blob)
			must(t, store.Begin(ctx, testSet, "publisher"))
			if phase != Publishing {
				must(t, store.ReleasePublisher(ctx, testSet, "publisher"))
			}
			if phase == Deleting || phase == Deleted {
				must(t, store.MarkDeleting(ctx, testSet))
			}
			if phase == Deleted {
				must(t, store.MarkDeleted(ctx, testSet))
			}
			r, err := store.RecoverReader(ReaderIdentity{SetID: testSet, RootID: "root", OwnerID: testOwner, ReaderID: uuid.NewString()})
			must(t, err)
			before := blob.puts
			must(t, r.Close(ctx))
			if v := inspect(t, store); v.Phase != phase || len(v.Readers) != 0 || len(v.Roots) != 0 || blob.puts != before+1 {
				t.Fatalf("close failed to fence without changing phase: %+v, puts %d -> %d", v, before, blob.puts)
			}
		})
	}
}

func TestReaderRecoveryFailsClosedWithoutValidCatalog(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		blob := newMemoryBlob()
		if corrupt {
			blob.objects[catalogKey(testSet)] = blobObject{data: []byte("{"), gen: 1}
		}
		r, err := New(blob).RecoverReader(ReaderIdentity{SetID: testSet, RootID: "root", OwnerID: testOwner, ReaderID: uuid.NewString()})
		must(t, err)
		want := ErrNotFound
		if corrupt {
			want = ErrInvalidCatalog
		}
		if err := r.Close(context.Background()); !errors.Is(err, want) {
			t.Fatalf("corrupt=%v: expected unresolved catalog, got %v", corrupt, err)
		}
		if blob.puts != 0 {
			t.Fatal("close created or replaced an invalid catalog")
		}
	}
}

func TestReaderPreparationAndRecoveryValidateWithoutIO(t *testing.T) {
	store := New(nil)
	r, err := store.PrepareReader(testSet, "root", testOwner)
	must(t, err)
	identity := r.Identity()
	if identity.SetID != testSet || identity.RootID != "root" || identity.OwnerID != testOwner {
		t.Fatalf("incorrect reserved tuple: %+v", identity)
	}
	must(t, validateUUID(identity.ReaderID))
	_, err = store.RecoverReader(identity)
	must(t, err)
	for _, mutate := range []func(*ReaderIdentity){
		func(id *ReaderIdentity) { id.SetID = "invalid" },
		func(id *ReaderIdentity) { id.RootID = "" },
		func(id *ReaderIdentity) { id.OwnerID = "invalid" },
		func(id *ReaderIdentity) { id.ReaderID = "invalid" },
	} {
		invalid := identity
		mutate(&invalid)
		if _, err := store.RecoverReader(invalid); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("invalid recovery identity accepted: %+v, %v", invalid, err)
		}
	}
}
