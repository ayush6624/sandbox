package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/chunkstore"
)

func TestOwnedBackupRecoversCloudCommitWithoutLocalSource(t *testing.T) {
	s, store, d, raw := seedOwnedHandoffBackup(t, true)
	ctx := context.Background()
	readerJournalSQL(t, s, `CREATE TRIGGER fail_backup_completion BEFORE UPDATE OF backup_complete ON hibernation_handoffs BEGIN SELECT RAISE(ABORT,'completion unavailable'); END`)
	if err := s.backupHandoff(ctx, d.Ref.Generation); err == nil {
		t.Fatal("lost local completion unexpectedly succeeded")
	}
	object, ok := uploadObject(store, d.artifactObject("record.json"))
	if !ok || len(object.data) == 0 {
		t.Fatal("fixture did not commit cloud descriptor")
	}
	before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
	readerJournalSQL(t, s, `DROP TRIGGER fail_backup_completion`)
	if err := os.RemoveAll(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); err != nil {
		t.Fatal(err)
	}
	s = restartChunkRetirementServer(t, s, store)
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatalf("cloud-complete recovery: %v", err)
	}
	if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
		t.Fatalf("recovery reuploaded payloads: %d -> %d", before, got)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || !view.Publisher.Released {
		t.Fatalf("completed publisher retained: %+v, %v", view, err)
	}
	staged, err := s.reconstructHandoff(ctx, d, filepath.Join(t.TempDir(), "restored.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	got, err := os.ReadFile(staged.MemPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("cloud recovery restored incorrect memory: %v", err)
	}
}

func cloudRecoveryPayloadAttempts(store *uploadTestStore, generation string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	n := 0
	for key, count := range store.puts {
		if strings.HasPrefix(key, chunkstore.DataPrefix(generation)) {
			n += count
		}
	}
	return n
}

func TestOwnedBackupRecoversWhenIndividualSourceFileIsMissing(t *testing.T) {
	for _, missing := range []string{"descriptor.json", "mem.bin", "state.bin", "rootfs.ext4"} {
		t.Run(missing, func(t *testing.T) {
			s, store, d, _ := seedOwnedHandoffBackup(t, true)
			readerJournalSQL(t, s, `CREATE TRIGGER fail_backup_completion BEFORE UPDATE OF backup_complete ON hibernation_handoffs BEGIN SELECT RAISE(ABORT,'completion unavailable'); END`)
			if err := s.backupHandoff(context.Background(), d.Ref.Generation); err == nil {
				t.Fatal("fixture did not lose local completion")
			}
			readerJournalSQL(t, s, `DROP TRIGGER fail_backup_completion`)
			if err := os.Remove(filepath.Join(s.hibPeerDir(), d.Ref.Generation, missing)); err != nil {
				t.Fatal(err)
			}
			before := cloudRecoveryPayloadAttempts(store, d.Ref.Generation)
			if err := s.backupHandoff(context.Background(), d.Ref.Generation); err != nil {
				t.Fatalf("recover after losing %s: %v", missing, err)
			}
			if got := cloudRecoveryPayloadAttempts(store, d.Ref.Generation); got != before {
				t.Fatalf("source preflight admitted payload writes: %d -> %d", before, got)
			}
			if missing == "descriptor.json" {
				if _, err := os.Stat(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); !os.IsNotExist(err) {
					t.Fatalf("recovery orphaned retained files without their descriptor: %v", err)
				}
			}
		})
	}
}

func TestOwnedBackupAddsReceiptToExistingBackupWithLocalSource(t *testing.T) {
	s, store, d, _ := seedOwnedHandoffBackup(t, true)
	ctx := context.Background()
	readerJournalSQL(t, s, `CREATE TRIGGER fail_backup_completion BEFORE UPDATE OF backup_complete ON hibernation_handoffs BEGIN SELECT RAISE(ABORT,'completion unavailable'); END`)
	if err := s.backupHandoff(ctx, d.Ref.Generation); err == nil {
		t.Fatal("fixture did not lose local completion")
	}
	readerJournalSQL(t, s, `DROP TRIGGER fail_backup_completion`)
	store.mu.Lock()
	delete(store.objects, d.artifactObject("backup-receipt.json"))
	before := make(map[string]int64)
	for name, object := range store.objects {
		if strings.HasPrefix(name, chunkstore.DataPrefix(d.Ref.Generation)) {
			before[name] = object.generation
		}
	}
	store.mu.Unlock()
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	for name, generation := range before {
		object, _ := uploadObject(store, name)
		if object.generation != generation {
			t.Fatalf("receipt upgrade rewrote existing payload %s", name)
		}
	}
	var receipt ownedBackupReceipt
	if err := s.readOwnedBackupMetadata(ctx, d.artifactObject("backup-receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if err := receipt.validateInventory(d); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedBackupMetadataDownloadCannotBypassSizeBound(t *testing.T) {
	body := ownedBackupMetadataBuffer{remaining: 4}
	// io.Copy uses ReaderFrom when the destination exposes it. The download
	// bound must also hold on that optimized path, not just direct Write calls.
	_, err := io.Copy(&body, io.LimitReader(strings.NewReader("too much metadata"), 100))
	if err == nil || body.buffer.Len() > 4 {
		t.Fatalf("metadata download exceeded its bound: len=%d err=%v", body.buffer.Len(), err)
	}
}
