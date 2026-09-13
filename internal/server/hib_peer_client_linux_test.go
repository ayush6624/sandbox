//go:build linux

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
)

func TestHibPeerClientSparseArtifactsAndCleanFallback(t *testing.T) {
	for _, form := range []string{rootfsFormFull, rootfsFormDiff} {
		for _, interrupted := range []bool{false, true} {
			name := form + "/peer"
			if interrupted {
				name = form + "/interrupted"
			}
			t.Run(name, func(t *testing.T) {
				pages := make([][]byte, 256)
				for i := range pages {
					pages[i] = make([]byte, 4096)
				}
				pages[0] = bytes.Repeat([]byte{19}, 4096)
				f := newHibPeerClientFixture(t, pages...)
				f.server.cfg.UFFDRestore = true
				f.record.RootfsForm = form
				ctx := context.Background()
				dir := t.TempDir()
				const size = 64 << 10
				const preserved = 16 << 10
				const changed = 48 << 10
				want := make([]byte, size)
				if form == rootfsFormDiff {
					f.record.RootfsBaseID = "peer-test-base"
					copy(want[preserved:], []byte("base bytes must survive failed overlay"))
					base := filepath.Join(dir, "base")
					if err := os.WriteFile(base, want, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := f.store.client.PutSparse(ctx, baseObj(f.record.RootfsBaseID, "rootfs.sz"), base); err != nil {
						t.Fatal(err)
					}
				}
				copy(want[changed:], []byte("durable disk tail"))
				root := filepath.Join(dir, "durable-root")
				if err := os.WriteFile(root, want, 0600); err != nil {
					t.Fatal(err)
				}
				rootRanges := []gcsblob.Range{{Off: changed, Len: 4096}}
				state := filepath.Join(dir, "state")
				stateBytes := []byte("saved device state")
				if err := os.WriteFile(state, stateBytes, 0600); err != nil {
					t.Fatal(err)
				}
				var stateWire, rootWire bytes.Buffer
				if _, err := gcsblob.WriteRanges(&stateWire, state, []gcsblob.Range{{Off: 0, Len: int64(len(stateBytes))}}); err != nil {
					t.Fatal(err)
				}
				if interrupted {
					bad := filepath.Join(dir, "bad-peer-root")
					badBytes := append([]byte(nil), want...)
					copy(badBytes[preserved:], bytes.Repeat([]byte{92}, 4096))
					if err := os.WriteFile(bad, badBytes, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := gcsblob.WriteRanges(&rootWire, bad, []gcsblob.Range{{Off: preserved, Len: 4096}, {Off: changed, Len: 4096}}); err != nil {
						t.Fatal(err)
					}
					rootWire.Truncate(rootWire.Len() - 8)
					probe := filepath.Join(dir, "partial-probe")
					if err := gcsblob.ReadSparse(bytes.NewReader(rootWire.Bytes()), probe); err == nil {
						t.Fatal("fixture did not interrupt sparse stream")
					}
					partial, err := os.ReadFile(probe)
					if err != nil || partial[preserved] != 92 {
						t.Fatalf("fixture did not write wrong bytes before failure: %v", err)
					}
					if _, err := f.store.client.PutRanges(ctx, hibRootfsObj(f.record.ID), root, rootRanges); err != nil {
						t.Fatal(err)
					}
					if _, err := f.store.client.PutSparse(ctx, hibStateObj(f.record.ID), state); err != nil {
						t.Fatal(err)
					}
					manifest, _ := json.Marshal(f.descriptor.Manifest)
					if err := f.store.client.PutBytes(ctx, hibManifestObj(f.record.ID), manifest); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := gcsblob.WriteRanges(&rootWire, root, rootRanges); err != nil {
						t.Fatal(err)
					}
					// A successful peer path cannot hide a cloud payload fetch.
					f.store.mu.Lock()
					delete(f.store.objects, chunkObj(testChunkHash(pages[0])))
					f.store.mu.Unlock()
				}
				f.mu.Lock()
				f.descriptor.Record = f.record
				f.descriptor.Record.Peer = nil
				f.artifacts["state"] = stateWire.Bytes()
				f.artifacts["rootfs"] = rootWire.Bytes()
				f.mu.Unlock()
				targetRoot := f.server.cfg.Provisioner.RootfsPathFor(f.record.ID)
				staged, err := f.server.reconstructHibArtifacts(ctx, &f.record, targetRoot)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(targetRoot)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("rootfs retained partial peer bytes or lost base: %v", err)
				}
				got, err = os.ReadFile(staged.StatePath)
				if err != nil || !bytes.Equal(got, stateBytes) {
					t.Fatalf("state mismatch: %v", err)
				}
				if staged.Chunks == nil || staged.MemPath != "" {
					t.Fatal("peer reconstruction eagerly materialized RAM")
				}
				got, err = staged.Chunks.Load(0)
				if err != nil || !bytes.Equal(got, pages[0]) {
					t.Fatalf("restored chunk: %v", err)
				}
				if interrupted && f.server.met.hibPeerFallbacks.Load() != 1 {
					t.Fatal("interrupted stream did not select cloud fallback")
				}
				if !interrupted && (f.server.met.hibPeerArtifacts.Load() != 2 || f.server.met.hibPeerChunks.Load() != 1) {
					t.Fatal("expected peer disk, state and lazy RAM bytes")
				}
				for _, path := range []string{staged.StatePath + ".peer.tmp", targetRoot + ".peer.tmp"} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("temporary artifact survived: %s: %v", path, err)
					}
				}
			})
		}
	}
}
