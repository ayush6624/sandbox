package registry

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func openChunkRootReleaseRegistry(t *testing.T, path string) *Registry {
	t.Helper()
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	return r
}

func TestChunkRootReleasePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	want := ChunkRootRelease{
		SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 17,
	}

	r := openChunkRootReleaseRegistry(t, path)
	if err := r.QueueChunkRootRelease(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r = openChunkRootReleaseRegistry(t, path)
	t.Cleanup(func() { _ = r.Close() })
	got, err := r.ListChunkRootReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ChunkRootRelease{want}) {
		t.Fatalf("reopened releases = %+v, want %+v", got, want)
	}
}

func TestChunkRootReleasePersistsAcrossProcessExit(t *testing.T) {
	const childDBEnv = "SANDBOX_TEST_CHUNK_ROOT_RELEASE_EXIT_DB"
	want := ChunkRootRelease{
		SetID: "set-exit", RootID: "root-exit", SandboxID: "sandbox-exit", Revision: 23, Fenced: true,
	}
	if path := os.Getenv(childDBEnv); path != "" {
		r := openChunkRootReleaseRegistry(t, path)
		if err := r.QueueChunkRootRelease(context.Background(), want); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "registry.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestChunkRootReleasePersistsAcrossProcessExit$")
	cmd.Env = append(os.Environ(), childDBEnv+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child process: %v\n%s", err, output)
	}
	r := openChunkRootReleaseRegistry(t, path)
	t.Cleanup(func() { _ = r.Close() })
	got, err := r.ListChunkRootReleases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ChunkRootRelease{want}) {
		t.Fatalf("releases after process exit = %+v, want %+v", got, want)
	}
}

func TestChunkRootReleaseWriterUsesFullSynchronous(t *testing.T) {
	r := testRegistry(t)
	var synchronous int
	if err := r.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 {
		t.Fatalf("PRAGMA synchronous = %d, want 2 (FULL)", synchronous)
	}
}

func TestChunkRootReleaseReplayPreservesRevisionAndFence(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	initial := ChunkRootRelease{
		SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 7,
	}
	for _, release := range []ChunkRootRelease{
		initial,
		{SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 99},
		{SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Fenced: true},
		{SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 101},
	} {
		if err := r.QueueChunkRootRelease(ctx, release); err != nil {
			t.Fatalf("queue %+v: %v", release, err)
		}
	}
	initial.Fenced = true
	got, err := r.ListChunkRootReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ChunkRootRelease{initial}) {
		t.Fatalf("replayed release = %+v, want %+v", got, initial)
	}
}

func TestChunkRootReleaseRejectsSandboxIdentityCollision(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	want := ChunkRootRelease{
		SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 3,
	}
	if err := r.QueueChunkRootRelease(ctx, want); err != nil {
		t.Fatal(err)
	}
	collision := want
	collision.SandboxID = "sandbox-b"
	collision.Revision = 4
	collision.Fenced = true
	if err := r.QueueChunkRootRelease(ctx, collision); !errors.Is(err, ErrChunkRootReleaseConflict) {
		t.Fatalf("collision error = %v, want %v", err, ErrChunkRootReleaseConflict)
	}
	got, err := r.ListChunkRootReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []ChunkRootRelease{want}) {
		t.Fatalf("collision changed release: %+v", got)
	}
}

func TestChunkRootReleaseIndependentRootsAndRemoval(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	releases := []ChunkRootRelease{
		{SetID: "set-a", RootID: "root-a", SandboxID: "sandbox-a", Revision: 1},
		{SetID: "set-a", RootID: "root-b", SandboxID: "sandbox-a", Revision: 2},
		{SetID: "set-b", RootID: "root-a", SandboxID: "sandbox-b", Revision: 3},
	}
	for _, release := range releases {
		if err := r.QueueChunkRootRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RemoveChunkRootRelease(ctx, "set-a", "root-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveChunkRootRelease(ctx, "set-a", "root-b"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	got, err := r.ListChunkRootReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ChunkRootRelease{releases[0], releases[2]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining releases = %+v, want %+v", got, want)
	}
}

func TestChunkRootReleaseValidation(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	valid := ChunkRootRelease{
		SetID: "set", RootID: "root", SandboxID: "sandbox", Revision: 1,
	}
	tests := []ChunkRootRelease{
		{RootID: valid.RootID, SandboxID: valid.SandboxID, Revision: 1},
		{SetID: valid.SetID, SandboxID: valid.SandboxID, Revision: 1},
		{SetID: valid.SetID, RootID: valid.RootID, Revision: 1},
		{SetID: valid.SetID, RootID: valid.RootID, SandboxID: valid.SandboxID},
		{SetID: valid.SetID, RootID: valid.RootID, SandboxID: valid.SandboxID, Revision: -1, Fenced: true},
	}
	for _, release := range tests {
		if err := r.QueueChunkRootRelease(ctx, release); !errors.Is(err, ErrInvalidChunkRootRelease) {
			t.Errorf("QueueChunkRootRelease(%+v) error = %v, want %v", release, err, ErrInvalidChunkRootRelease)
		}
	}
	if err := r.QueueChunkRootRelease(ctx, ChunkRootRelease{
		SetID: valid.SetID, RootID: valid.RootID, SandboxID: valid.SandboxID, Fenced: true,
	}); err != nil {
		t.Fatalf("fenced release without revision: %v", err)
	}
	for _, ids := range [][2]string{{"", "root"}, {"set", ""}} {
		if err := r.RemoveChunkRootRelease(ctx, ids[0], ids[1]); !errors.Is(err, ErrInvalidChunkRootRelease) {
			t.Errorf("RemoveChunkRootRelease(%q, %q) error = %v", ids[0], ids[1], err)
		}
	}
}
