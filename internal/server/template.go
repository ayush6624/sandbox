package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

// A template is a snapshot, and this is the one thing that could not already be
// expressed through the public API: booting a rootfs that is not the host's
// configured base. `sandbox template build` converts a container image into an
// ext4 file (cmd/sandbox/template.go), calls this, and gets back a snapshot id
// — from there the existing machinery does everything else. Fan-out clones it,
// the GCS pull makes it usable on any worker, and `POST /v1/sandboxes` with
// `source: {type: "snapshot", id}` creates from it.
//
// The route is worker-local by construction: the gateway proxies an explicit
// list of paths and /templates is not one of them, so a tenant cannot reach it
// and cannot name a host path to boot.

// templateInitPath is where the build overlays sandboxd, and what the template
// boot passes to the kernel as init=.
const templateInitPath = agentapi.AgentPath

type templateBuildRequest struct {
	// RootfsPath is a host-local ext4 image with sandboxd overlaid at
	// templateInitPath. The build copies it; the caller owns deleting it.
	RootfsPath string `json:"rootfs_path"`
	Name       string `json:"name"`
	// Firecracker bakes vcpus/mem into a snapshot and restores reject
	// overrides, so what is chosen here is what every sandbox from this
	// template gets. 0 = the host template's default.
	Vcpus  int64 `json:"vcpus"`
	MemMIB int64 `json:"mem_mib"`
}

// handleTemplateBuild boots the supplied rootfs once, snapshots the running
// guest, and destroys it — the same sequence buildGolden uses, minus the golden
// marking. It returns the snapshot.
func (s *Server) handleTemplateBuild(w http.ResponseWriter, r *http.Request) {
	var body templateBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if !filepath.IsAbs(body.RootfsPath) {
		httpError(w, 400, errors.New("rootfs_path must be an absolute host path"))
		return
	}
	if _, err := os.Stat(body.RootfsPath); err != nil {
		httpError(w, 400, fmt.Errorf("rootfs_path: %w", err))
		return
	}
	if err := s.validateResources(body.Vcpus, body.MemMIB); err != nil {
		httpError(w, 400, err)
		return
	}

	t0 := time.Now()
	// -1: never hibernate the build VM out from under the snapshot step.
	sb, err := s.createCold(r.Context(), body.Name, nil, -1, body.Vcpus, body.MemMIB, body.RootfsPath)
	if err != nil {
		httpError(w, 500, fmt.Errorf("boot template rootfs: %w", err))
		return
	}
	snap, _, snapErr := s.snapshotSandboxWithRole(r.Context(), sb.ID, false, registry.SnapshotRoleTemplate, body.Name, nil)
	// The build VM exists only to be snapshotted — destroy it either way.
	if err := s.destroy(context.Background(), sb.ID); err != nil {
		fmt.Fprintf(os.Stderr, "template build: destroy source %s: %v\n", sb.ID, err)
	}
	if snapErr != nil {
		httpError(w, 500, fmt.Errorf("snapshot template: %w", snapErr))
		return
	}
	fmt.Fprintf(os.Stderr, "template snapshot %s built in %s\n", snap.ID, time.Since(t0).Round(time.Millisecond))
	writeJSON(w, 201, snap)
}
