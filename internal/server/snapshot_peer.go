package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

const peerSnapshotTimeout = 5 * time.Minute

var peerSnapshotClient = &http.Client{
	Transport: client.SharedTransport(),
	// Never forward the worker bearer to a redirected address. Peer hints are
	// private-IP validated, but a redirect would otherwise bypass that boundary.
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
}

// handlePeerSnapshotMeta exposes immutable snapshot metadata to another
// authenticated worker. The /internal/v1 prefix makes bearerAuth require the
// worker credential rather than a tenant API token.
func (s *Server) handlePeerSnapshotMeta(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	op := s.snapshotLock(id)
	op.RLock()
	defer op.RUnlock()

	snap, err := s.reg.GetSnapshot(r.Context(), id)
	if err != nil || snap.Golden {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handlePeerSnapshotArtifact streams one artifact in the same gzip sparse
// representation used in GCS. For a diff rootfs only extents changed from the
// immutable base are sent, so a peer transfer retains the bucket path's byte
// efficiency while removing the object-store round trip.
func (s *Server) handlePeerSnapshotArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	artifact := r.PathValue("artifact")
	op := s.snapshotLock(id)
	op.RLock()
	defer op.RUnlock()

	snap, err := s.reg.GetSnapshot(r.Context(), id)
	if err != nil || snap.Golden {
		http.NotFound(w, r)
		return
	}

	var artifactPath string
	var ranges []gcsblob.Range
	switch artifact {
	case "mem":
		artifactPath = snap.MemPath
	case "state":
		artifactPath = snap.StatePath
	case "rootfs":
		artifactPath = snap.RootfsPath
		if snap.Format == registry.FormatDiff {
			base, baseErr := s.reg.GetSnapshot(r.Context(), snap.BaseID)
			if baseErr != nil {
				httpError(w, http.StatusConflict, fmt.Errorf("snapshot base %s: %w", snap.BaseID, baseErr))
				return
			}
			diff, diffErr := s.cfg.Provisioner.DiffExtents(snap.RootfsPath, base.RootfsPath)
			if diffErr != nil {
				httpError(w, http.StatusInternalServerError, fmt.Errorf("diff rootfs extents: %w", diffErr))
				return
			}
			ranges = toBlobRanges(diff)
		}
	default:
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.sandbox.sparse")
	var payload int64
	if artifact == "rootfs" && snap.Format == registry.FormatDiff {
		payload, err = gcsblob.WriteRanges(w, artifactPath, ranges)
	} else {
		payload, err = gcsblob.WriteSparse(w, artifactPath)
	}
	if err != nil {
		// Headers may already be committed; the peer's gzip decoder will reject a
		// truncated stream and fall back to GCS. Keep the source-side diagnosis.
		fmt.Fprintf(os.Stderr, "[snapshot %s] peer stream %s failed: %v\n", id, artifact, err)
		return
	}
	s.met.snapshotPeerServes.Add(1)
	s.met.snapshotPeerBytes.Add(payload)
}

func snapshotPeerBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("peer URL must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("peer URL must contain only scheme, private host, and port")
	}
	host := u.Hostname()
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("peer host must be an IP address: %w", err)
	}
	if !ip.IsPrivate() && !ip.IsLoopback() {
		return "", fmt.Errorf("peer IP %s is not private", ip)
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("invalid peer port %q", port)
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (s *Server) pullSnapshotFromPeer(ctx context.Context, snapID, rawPeer string) (registry.Snapshot, int64, error) {
	peer, err := snapshotPeerBase(rawPeer)
	if err != nil {
		return registry.Snapshot{}, 0, err
	}
	if s.workerCredentials == nil {
		return registry.Snapshot{}, 0, errors.New("worker credential unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, peerSnapshotTimeout)
	defer cancel()

	basePath := peer + "/internal/v1/snapshots/" + url.PathEscape(snapID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, basePath, nil)
	if err != nil {
		return registry.Snapshot{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.workerCredentials.Outbound())
	resp, err := peerSnapshotClient.Do(req)
	if err != nil {
		return registry.Snapshot{}, 0, fmt.Errorf("fetch metadata: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return registry.Snapshot{}, 0, fmt.Errorf("fetch metadata: HTTP %d", resp.StatusCode)
	}
	var meta registry.Snapshot
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta)
	_ = resp.Body.Close()
	if decodeErr != nil {
		return registry.Snapshot{}, 0, fmt.Errorf("decode metadata: %w", decodeErr)
	}
	if meta.ID != snapID || meta.Golden {
		return registry.Snapshot{}, 0, errors.New("peer returned mismatched or built-in snapshot metadata")
	}
	if meta.Format == "" {
		meta.Format = registry.FormatFull
	}
	if meta.Format != registry.FormatFull && meta.Format != registry.FormatDiff {
		return registry.Snapshot{}, 0, fmt.Errorf("unsupported snapshot format %q", meta.Format)
	}
	if meta.Format == registry.FormatDiff && meta.BaseID == "" {
		return registry.Snapshot{}, 0, errors.New("diff snapshot has no base id")
	}

	memPath, statePath, rootfsPath, err := s.cfg.Provisioner.SnapshotPaths(snapID)
	if err != nil {
		return registry.Snapshot{}, 0, err
	}
	memTmp, stateTmp, rootfsTmp := memPath+".peer.tmp", statePath+".peer.tmp", rootfsPath+".peer.tmp"
	temps := []string{memTmp, stateTmp, rootfsTmp}
	for _, name := range temps {
		_ = os.Remove(name)
	}
	committed := false
	defer func() {
		if !committed {
			for _, name := range temps {
				_ = os.Remove(name)
			}
		}
	}()

	if meta.Format == registry.FormatDiff {
		_, baseRootfs, err := s.ensureBaseLocal(ctx, meta.BaseID)
		if err != nil {
			return registry.Snapshot{}, 0, fmt.Errorf("stage base %s: %w", meta.BaseID, err)
		}
		if err := provisioner.CloneFile(baseRootfs, rootfsTmp); err != nil {
			return registry.Snapshot{}, 0, fmt.Errorf("stage base rootfs: %w", err)
		}
	}

	var transferred int64
	for _, artifact := range []struct {
		name string
		dst  string
	}{
		{name: "rootfs", dst: rootfsTmp},
		{name: "mem", dst: memTmp},
		{name: "state", dst: stateTmp},
	} {
		got, err := s.fetchPeerSparse(ctx, basePath+"/"+path.Base(artifact.name), artifact.dst)
		transferred += got
		if err != nil {
			return registry.Snapshot{}, transferred, fmt.Errorf("fetch %s: %w", artifact.name, err)
		}
	}

	finals := []struct{ tmp, dst string }{{rootfsTmp, rootfsPath}, {memTmp, memPath}, {stateTmp, statePath}}
	for index, file := range finals {
		if err := os.Rename(file.tmp, file.dst); err != nil {
			for _, prior := range finals[:index] {
				_ = os.Remove(prior.dst)
			}
			return registry.Snapshot{}, transferred, err
		}
	}
	committed = true

	row := meta
	row.MemPath, row.StatePath, row.RootfsPath = memPath, statePath, rootfsPath
	row.Golden = false
	if err := s.reg.CreateSnapshot(ctx, row); err != nil {
		_ = s.cfg.Provisioner.CleanupSnapshot(snapID)
		return registry.Snapshot{}, transferred, fmt.Errorf("record peer snapshot: %w", err)
	}
	return row, transferred, nil
}

type byteCountingReader struct {
	r io.Reader
	n int64
}

func (r *byteCountingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func (s *Server) fetchPeerSparse(ctx context.Context, endpoint, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.workerCredentials.Outbound())
	resp, err := peerSnapshotClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	counted := &byteCountingReader{r: resp.Body}
	if err := gcsblob.ReadSparse(counted, dst); err != nil {
		return counted.n, err
	}
	return counted.n, nil
}

// snapshotPeerHint returns the gateway-supplied peer address. Validation is
// deliberately deferred until a local miss so an irrelevant malformed header
// cannot make a local restore fail.
func snapshotPeerHint(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(cluster.SnapshotPeerHeader))
}
