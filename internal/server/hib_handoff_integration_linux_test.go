//go:build linux

package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
)

func TestHandoffReleaseAndPeerReadBeforeBlockedBackupCompletes(t *testing.T) {
	source, store, sb, files := seedPeerRelease(t)
	creds, err := management.NewCredentials([]string{"handoff-test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	source.workerCredentials = creds
	mux := http.NewServeMux()
	for _, pattern := range []string{"GET /internal/v1/hibernations/{generation}", "DELETE /internal/v1/hibernations/{generation}", "GET /internal/v1/hibernations/{generation}/{artifact}", "GET /internal/v1/hibernations/{generation}/chunks/{hash}"} {
		mux.HandleFunc(pattern, source.handlePeerHibernation)
	}
	peer := httptest.NewServer(bearerAuth(nil, creds, mux))
	defer peer.Close()
	source.cfg.AdvertiseAddr = peer.URL
	ref, err := source.releaseForHandoff(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.reg.Get(context.Background(), sb.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("source still serving: %v", err)
	}
	replay, err := source.releaseForHandoff(context.Background(), sb.ID)
	if err != nil || replay.Generation != ref.Generation {
		t.Fatalf("release retry changed generation: %+v %v", replay, err)
	}
	if _, err := source.blob.GetBytes(context.Background(), handoffRecordObj(sb.ID, ref.Generation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release waited for backup: %v", err)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.hook(func(_ *http.Request, object string) int {
		if object == handoffStateObj(sb.ID, ref.Generation) {
			once.Do(func() { close(entered); <-unblock })
		}
		return 0
	})
	done := make(chan error, 1)
	go func() { done <- source.backupHandoff(context.Background(), ref.Generation) }()
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backup never reached blocked payload")
	}
	target, _ := testLifecycleServer(t)
	target.cfg.HostID = "handoff-target"
	target.cfg.UFFDRestore = true
	target.cfg.Provisioner.RootfsDir = t.TempDir()
	target.blob, target.workerCredentials = store.client, creds
	claim, err := target.claimHandoff(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := target.reconstructHandoff(context.Background(), claim.Control.Descriptor, target.cfg.Provisioner.RootfsPathFor(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	if staged.Peer == nil || staged.Chunks == nil {
		t.Fatal("handoff did not retain lazy peer source")
	}
	data, err := staged.Chunks.Load(0)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{23}, 1<<20)) {
		t.Fatalf("peer memory before backup: %v", err)
	}
	if err := target.authorizeHandoffRun(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	other, _ := testLifecycleServer(t)
	other.blob = store.client
	other.cfg.HostID = "staggered-target"
	if _, err := other.claimHandoff(context.Background(), sb.ID); !errors.Is(err, ErrOwnerContended) {
		t.Fatalf("second target got running generation: %v", err)
	}
	if err := staged.Peer.hydrateAndAcknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := source.reg.GetHibernationHandoff(context.Background(), ref.Generation)
	if err != nil || !job.CacheReady || job.BackupComplete {
		t.Fatalf("ack erased pending backup: %+v %v", job, err)
	}
	if _, err := os.Stat(filepath.Join(source.hibPeerDir(), ref.Generation, "mem.bin")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	if err := source.awaitHandoffBackup(ctx, sb.ID, ref.Generation); err == nil {
		t.Fatal("durable wait returned during blocked upload")
	}
	cancel()
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source.hibPeerDir(), ref.Generation)); !os.IsNotExist(err) {
		t.Fatalf("completed source not cleaned: %v", err)
	}
	// A fresh destination must use the exact generation backup after the peer
	// files disappear, even though legacy cloud objects also exist in this fixture.
	other.cfg.UFFDRestore = true
	other.cfg.Provisioner.RootfsDir = t.TempDir()
	other.workerCredentials = creds
	restored, err := other.reconstructHandoff(context.Background(), claim.Control.Descriptor, other.cfg.Provisioner.RootfsPathFor(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	data, err = restored.Chunks.Load(0)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{23}, 1<<20)) {
		t.Fatalf("generation cloud fallback: %v", err)
	}
	_, originalState, _, err := source.cfg.Provisioner.SnapshotPaths(hibID(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(restored.StatePath)
	if err != nil || !bytes.Equal(got, files[originalState]) {
		t.Fatalf("generation state: %v", err)
	}
}
