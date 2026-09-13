package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

type cloudValidationPayload struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
}

type cloudValidationReceipt struct {
	Version          int                      `json:"version"`
	DescriptorSHA256 string                   `json:"descriptor_sha256"`
	Payloads         []cloudValidationPayload `json:"payloads"`
	Base             *cloudValidationPayload  `json:"base,omitempty"`
}

func seedCloudValidationBackup(t *testing.T) (*Server, *uploadTestStore, *retainedHibernation) {
	t.Helper()
	s, store, d, _ := seedOwnedHandoffBackup(t, true)
	loseCloudValidationSource(t, s, store, d)
	return s, store, d
}

func loseCloudValidationSource(t *testing.T, s *Server, store *uploadTestStore, d *retainedHibernation) {
	t.Helper()
	readerJournalSQL(t, s, `CREATE TRIGGER fail_backup_completion BEFORE UPDATE OF backup_complete ON hibernation_handoffs BEGIN SELECT RAISE(ABORT,'completion unavailable'); END`)
	if err := s.backupHandoff(context.Background(), d.Ref.Generation); err == nil {
		t.Fatal("fixture did not lose local completion")
	}
	for _, name := range []string{"backup-receipt.json", "record.json"} {
		obj, ok := uploadObject(store, d.artifactObject(name))
		if !ok || len(obj.data) == 0 {
			t.Fatalf("fixture did not commit %s", name)
		}
	}
	readerJournalSQL(t, s, `DROP TRIGGER fail_backup_completion`)
	if err := os.RemoveAll(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); err != nil {
		t.Fatal(err)
	}
}

func replaceCloudValidationObject(store *uploadTestStore, name string, data []byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.generation++
	store.objects[name] = uploadTestObject{data: data, generation: store.generation}
}

func changeCloudValidationReceipt(t *testing.T, store *uploadTestStore, d *retainedHibernation, change func(*cloudValidationReceipt)) {
	t.Helper()
	name := d.artifactObject("backup-receipt.json")
	obj, ok := uploadObject(store, name)
	if !ok {
		t.Fatal("missing receipt fixture")
	}
	var receipt cloudValidationReceipt
	if err := json.Unmarshal(obj.data, &receipt); err != nil {
		t.Fatal(err)
	}
	change(&receipt)
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	replaceCloudValidationObject(store, name, data)
}

func assertCloudValidationPending(t *testing.T, s *Server, d *retainedHibernation, released bool) {
	t.Helper()
	ctx := context.Background()
	job, err := s.reg.GetHibernationHandoff(ctx, d.Ref.Generation)
	if err != nil || job.BackupComplete {
		t.Fatalf("invalid cloud evidence completed or removed job: %+v, %v", job, err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || view.Publisher.Released != released {
		t.Fatalf("invalid cloud evidence changed publisher: %+v, %v", view.Publisher, err)
	}
}

func TestOwnedCloudRecoveryRejectsInvalidEvidence(t *testing.T) {
	type mutation func(*testing.T, *uploadTestStore, *retainedHibernation)
	cases := map[string]mutation{}
	for _, name := range []string{"record.json", "backup-receipt.json"} {
		cases[name+" missing"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			store.mu.Lock()
			delete(store.objects, d.artifactObject(name))
			store.mu.Unlock()
		}
		cases[name+" trailing JSON"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			obj, _ := uploadObject(store, d.artifactObject(name))
			body := append(append([]byte(nil), obj.data...), []byte(" {}")...)
			replaceCloudValidationObject(store, d.artifactObject(name), body)
		}
		cases[name+" oversized"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			replaceCloudValidationObject(store, d.artifactObject(name), make([]byte, (16<<20)+1))
		}
		for label, body := range map[string]string{"empty": "", "malformed": "{", "null": "null"} {
			cases[name+" "+label] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
				replaceCloudValidationObject(store, d.artifactObject(name), []byte(body))
			}
		}
	}
	cases["record differs from journal"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
		wrong := *d
		wrong.Record.Name += "-different"
		body, err := json.Marshal(wrong)
		if err != nil {
			t.Fatal(err)
		}
		replaceCloudValidationObject(store, d.artifactObject("record.json"), body)
	}
	for name, change := range map[string]func(*cloudValidationReceipt){
		"unsupported receipt version": func(r *cloudValidationReceipt) { r.Version++ },
		"wrong descriptor digest":     func(r *cloudValidationReceipt) { r.DescriptorSHA256 = strings.Repeat("0", 64) },
		"missing payload":             func(r *cloudValidationReceipt) { r.Payloads = r.Payloads[1:] },
		"duplicate payload":           func(r *cloudValidationReceipt) { r.Payloads = append(r.Payloads, r.Payloads[0]) },
		"unexpected payload": func(r *cloudValidationReceipt) {
			r.Payloads = append(r.Payloads, cloudValidationPayload{Name: "unexpected", Generation: 123})
		},
		"zero generation":     func(r *cloudValidationReceipt) { r.Payloads[0].Generation = 0 },
		"negative generation": func(r *cloudValidationReceipt) { r.Payloads[0].Generation = -1 },
		"escaped payload":     func(r *cloudValidationReceipt) { r.Payloads[0].Name = "../state.sz" },
		"unexpected base for full rootfs": func(r *cloudValidationReceipt) {
			r.Base = &cloudValidationPayload{Name: "bases/unexpected/rootfs.sz", Generation: 123}
		},
	} {
		cases[name] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			changeCloudValidationReceipt(t, store, d, change)
		}
	}
	for _, name := range []string{"record.json", "backup-receipt.json"} {
		cases["metadata included as payload "+name] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			obj, _ := uploadObject(store, d.artifactObject(name))
			changeCloudValidationReceipt(t, store, d, func(r *cloudValidationReceipt) {
				r.Payloads = append(r.Payloads, cloudValidationPayload{Name: name, Generation: obj.generation})
			})
		}
	}
	for _, artifact := range []string{"chunk", "state.sz", "rootfs.sz"} {
		key := func(d *retainedHibernation) string {
			if artifact == "chunk" {
				return d.Manifest.chunkObject(d.Manifest.Chunks[0].Hash)
			}
			return d.artifactObject(artifact)
		}
		cases[artifact+" missing"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			store.mu.Lock()
			delete(store.objects, key(d))
			store.mu.Unlock()
		}
		cases[artifact+" empty"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			replaceCloudValidationObject(store, key(d), nil)
		}
		cases[artifact+" same-size replacement"] = func(t *testing.T, store *uploadTestStore, d *retainedHibernation) {
			obj, ok := uploadObject(store, key(d))
			if !ok || len(obj.data) == 0 {
				t.Fatal("missing replacement fixture")
			}
			changed := append([]byte(nil), obj.data...)
			changed[len(changed)/2] ^= 255
			replaceCloudValidationObject(store, key(d), changed)
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, store, d := seedCloudValidationBackup(t)
			mutate(t, store, d)
			before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
			if err := s.backupHandoff(context.Background(), d.Ref.Generation); err == nil {
				t.Fatal("invalid cloud evidence completed backup")
			}
			assertCloudValidationPending(t, s, d, false)
			if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
				t.Fatalf("failed verification attempted payload writes: %d -> %d", before, got)
			}
		})
	}
}

func TestOwnedCloudRecoveryRequiresUnreleasedPublisher(t *testing.T) {
	s, store, d := seedCloudValidationBackup(t)
	ctx := context.Background()
	catalog, _ := s.chunkStorage()
	if err := catalog.ReleasePublisher(ctx, d.Ref.Generation, handoffPublisherID(d.Ref.Generation)); err != nil {
		t.Fatal(err)
	}
	var payloadReads atomic.Int64
	store.mu.Lock()
	store.beforeGet = func(_ *http.Request, name string) {
		if strings.HasPrefix(name, "chunksets/data/") {
			payloadReads.Add(1)
		}
	}
	store.mu.Unlock()
	if err := s.backupHandoff(ctx, d.Ref.Generation); err == nil {
		t.Fatal("released publisher authorized cloud recovery")
	}
	assertCloudValidationPending(t, s, d, true)
	if got := payloadReads.Load(); got != 0 {
		t.Fatalf("released publisher admitted %d payload reads", got)
	}
}

func TestOwnedCloudRecoveryWithRetiredRootKeepsPublisherProof(t *testing.T) {
	s, store, d := seedCloudValidationBackup(t)
	ctx := context.Background()
	catalog, _ := s.chunkStorage()
	if err := catalog.RetireRoot(ctx, d.Ref.Generation, d.Manifest.Storage.RootID); err != nil {
		t.Fatal(err)
	}
	before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatalf("unreleased publisher failed to protect retired-root recovery: %v", err)
	}
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || !view.Publisher.Released {
		t.Fatalf("retired-root recovery did not release publisher: %+v, %v", view, err)
	}
	if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
		t.Fatalf("retired-root recovery attempted writes: %d -> %d", before, got)
	}
}

func TestOwnedCloudRecoveryConditionalMetadataReads(t *testing.T) {
	for _, artifact := range []string{"record.json", "backup-receipt.json"} {
		for _, action := range []string{"replace", "cancel"} {
			t.Run(artifact+" "+action, func(t *testing.T) {
				s, store, d := seedCloudValidationBackup(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var intercepted atomic.Bool
				store.mu.Lock()
				store.beforeGet = func(r *http.Request, name string) {
					if name != d.artifactObject(artifact) || r.URL.Query().Get("alt") != "media" || !intercepted.CompareAndSwap(false, true) {
						return
					}
					if action == "cancel" {
						cancel()
						return
					}
					obj, _ := uploadObject(store, name)
					replaceCloudValidationObject(store, name, obj.data)
				}
				store.mu.Unlock()
				before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
				if err := s.backupHandoff(ctx, d.Ref.Generation); err == nil {
					t.Fatal("interrupted conditional evidence read completed backup")
				}
				if !intercepted.Load() {
					t.Fatal("fixture never reached conditional evidence read")
				}
				assertCloudValidationPending(t, s, d, false)
				if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
					t.Fatalf("interrupted verification attempted writes: %d -> %d", before, got)
				}
			})
		}
	}
}

func seedCloudValidationDiffBackup(t *testing.T) (*Server, *uploadTestStore, *retainedHibernation, []byte) {
	t.Helper()
	base := bytes.Repeat([]byte("base"), 2048)
	want := append([]byte(nil), base...)
	copy(want[:4096], bytes.Repeat([]byte("diff"), 1024))
	s, store, d, _ := seedOwnedHandoffBackupWithDescriptor(t, true, func(s *Server, store *uploadTestStore, d *retainedHibernation) {
		d.Record.RootfsForm = rootfsFormDiff
		d.Record.RootfsBaseID = uuid.NewString()
		d.RootfsRanges = []gcsblob.Range{{Off: 0, Len: 4096}}
		basePath := filepath.Join(t.TempDir(), "base.ext4")
		if err := os.WriteFile(basePath, base, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.client.PutSparse(context.Background(), baseObj(d.Record.RootfsBaseID, "rootfs.sz"), basePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.hibPeerDir(), d.Ref.Generation, "rootfs.ext4"), want, 0600); err != nil {
			t.Fatal(err)
		}
	})
	loseCloudValidationSource(t, s, store, d)
	return s, store, d, want
}

func TestOwnedCloudRecoveryChecksDiffBase(t *testing.T) {
	for _, state := range []string{"original", "missing", "empty", "replacement", "receipt missing base", "receipt wrong base", "receipt invalid base generation"} {
		t.Run(state, func(t *testing.T) {
			s, store, d, want := seedCloudValidationDiffBackup(t)
			key := baseObj(d.Record.RootfsBaseID, "rootfs.sz")
			base, ok := uploadObject(store, key)
			if !ok || len(base.data) == 0 {
				t.Fatal("fixture did not upload its base")
			}
			receiptObject, _ := uploadObject(store, d.artifactObject("backup-receipt.json"))
			var receipt cloudValidationReceipt
			if err := json.Unmarshal(receiptObject.data, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Base == nil || receipt.Base.Name != key || receipt.Base.Generation != base.generation {
				t.Fatalf("writer did not capture exact base generation: %+v", receipt.Base)
			}
			switch state {
			case "missing":
				store.mu.Lock()
				delete(store.objects, key)
				store.mu.Unlock()
			case "empty":
				replaceCloudValidationObject(store, key, nil)
			case "replacement":
				changed := append([]byte(nil), base.data...)
				changed[len(changed)/2] ^= 255
				replaceCloudValidationObject(store, key, changed)
			case "receipt missing base":
				changeCloudValidationReceipt(t, store, d, func(r *cloudValidationReceipt) { r.Base = nil })
			case "receipt wrong base":
				changeCloudValidationReceipt(t, store, d, func(r *cloudValidationReceipt) { r.Base.Name = "bases/wrong/rootfs.sz" })
			case "receipt invalid base generation":
				changeCloudValidationReceipt(t, store, d, func(r *cloudValidationReceipt) { r.Base.Generation = 0 })
			}
			before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
			err := s.backupHandoff(context.Background(), d.Ref.Generation)
			if state != "original" {
				if err == nil {
					t.Fatal("invalid diff-base evidence completed cloud backup")
				}
				assertCloudValidationPending(t, s, d, false)
			} else {
				if err != nil {
					t.Fatalf("complete diff backup recovery: %v", err)
				}
				rootfs := filepath.Join(t.TempDir(), "restored.ext4")
				if runtime.GOOS == "linux" {
					staged, err := s.reconstructHandoff(context.Background(), d, rootfs)
					if err != nil {
						t.Fatal(err)
					}
					defer staged.Close()
				} else {
					// CloneFile requires GNU cp. Exercise the same sparse overlay
					// decoder here; Linux runs the complete restore above.
					if err := store.client.GetSparse(context.Background(), key, rootfs); err != nil {
						t.Fatal(err)
					}
					if err := store.client.GetSparse(context.Background(), d.artifactObject("rootfs.sz"), rootfs); err != nil {
						t.Fatal(err)
					}
				}
				got, err := os.ReadFile(rootfs)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("diff backup restored wrong rootfs: %d bytes, %v", len(got), err)
				}
			}
			if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
				t.Fatalf("diff recovery attempted payload writes: %d -> %d", before, got)
			}
		})
	}
}
