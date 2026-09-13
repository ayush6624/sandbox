package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/registry"
)

const ownedBackupMetadataLimit = 16 << 20

var errHandoffSourceMissing = errors.New("local handoff source is missing")

type ownedBackupPayloadReceipt struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
}

type ownedBackupReceipt struct {
	Version          int                         `json:"version"`
	DescriptorSHA256 string                      `json:"descriptor_sha256"`
	Payloads         []ownedBackupPayloadReceipt `json:"payloads"`
	Base             *ownedBackupPayloadReceipt  `json:"base,omitempty"`
}

func ownedBackupDescriptorDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *Server) writeOwnedBackupReceipt(ctx context.Context, w *chunkstore.Writer, d *retainedHibernation, receipt *ownedBackupReceipt) error {
	if d.Record.RootfsForm == rootfsFormDiff {
		info, err := s.blob.Stat(ctx, baseObj(d.Record.RootfsBaseID, "rootfs.sz"))
		if err != nil {
			return err
		}
		if info.Size == 0 {
			return errors.New("handoff rootfs base is empty")
		}
		receipt.Base = &ownedBackupPayloadReceipt{Name: info.Name, Generation: info.Generation}
	}
	if err := receipt.validateInventory(d); err != nil {
		return err
	}
	sort.Slice(receipt.Payloads, func(i, j int) bool { return receipt.Payloads[i].Name < receipt.Payloads[j].Name })
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if len(data) > ownedBackupMetadataLimit {
		return errors.New("handoff receipt exceeds backup metadata limit")
	}
	_, err = w.PutBytes(ctx, "backup-receipt.json", data)
	return err
}

func (r *ownedBackupReceipt) validateInventory(d *retainedHibernation) error {
	if r.Version != 1 {
		return errors.New("unsupported handoff backup receipt version")
	}
	expected := map[string]bool{"state.sz": true, "rootfs.sz": true}
	for _, chunk := range d.Manifest.Chunks {
		if chunk.Hash != chunkZeroHash {
			expected["chunks/"+chunk.Hash] = true
		}
	}
	for _, payload := range r.Payloads {
		if !expected[payload.Name] || payload.Generation <= 0 {
			return fmt.Errorf("invalid handoff payload receipt %q", payload.Name)
		}
		delete(expected, payload.Name)
	}
	if len(expected) != 0 {
		return errors.New("handoff receipt is missing payloads")
	}
	switch d.Record.RootfsForm {
	case rootfsFormDiff:
		if d.Record.RootfsBaseID == "" || r.Base == nil || r.Base.Name != baseObj(d.Record.RootfsBaseID, "rootfs.sz") || r.Base.Generation <= 0 {
			return errors.New("handoff receipt rootfs base mismatch")
		}
	case rootfsFormFull:
		if r.Base != nil {
			return errors.New("full handoff receipt contains a rootfs base")
		}
	default:
		return errors.New("invalid handoff rootfs form")
	}
	return nil
}

// The exclusive handoff attempt and unreleased publisher protect every object
// throughout verification. Receipts bind the original accepted immutable GCS
// generations; current nonempty objects alone cannot establish that identity.
func (s *Server) verifyOwnedHandoffBackup(ctx context.Context, job registry.HibernationHandoff) error {
	d, err := handoffJobDescriptor(job)
	if err != nil {
		return err
	}
	if d == nil || d.Manifest.Storage == nil {
		return errors.New("missing-source recovery requires an owned handoff")
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Manifest.Storage.SetID)
	if err != nil {
		return err
	}
	if view.Publisher.ID != handoffPublisherID(job.Generation) || view.Publisher.Released {
		return errors.New("handoff backup publisher is not protecting recovery")
	}
	var record retainedHibernation
	if err := s.readOwnedBackupMetadata(ctx, d.artifactObject("record.json"), &record); err != nil {
		return err
	}
	want, err := json.Marshal(d)
	if err != nil {
		return err
	}
	got, err := json.Marshal(&record)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return errors.New("cloud handoff descriptor differs from journal offer")
	}
	var receipt ownedBackupReceipt
	if err := s.readOwnedBackupMetadata(ctx, d.artifactObject("backup-receipt.json"), &receipt); err != nil {
		return err
	}
	if receipt.DescriptorSHA256 != ownedBackupDescriptorDigest(want) {
		return errors.New("cloud handoff receipt descriptor mismatch")
	}
	if err := receipt.validateInventory(d); err != nil {
		return err
	}
	for _, payload := range receipt.Payloads {
		if err := s.verifyOwnedBackupObject(ctx, d.artifactObject(payload.Name), payload.Generation); err != nil {
			return err
		}
	}
	if receipt.Base != nil {
		if err := s.verifyOwnedBackupObject(ctx, receipt.Base.Name, receipt.Base.Generation); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Server) verifyOwnedBackupObject(ctx context.Context, name string, generation int64) error {
	info, err := s.blob.Stat(ctx, name)
	if err != nil {
		return err
	}
	if info.Generation != generation || info.Size == 0 {
		return fmt.Errorf("handoff backup object %s no longer matches its upload receipt", name)
	}
	return nil
}

func (s *Server) readOwnedBackupMetadata(ctx context.Context, name string, dst any) error {
	info, err := s.blob.Stat(ctx, name)
	if err != nil {
		return err
	}
	if info.Size == 0 || info.Size > ownedBackupMetadataLimit {
		return fmt.Errorf("invalid handoff backup metadata size for %s", name)
	}
	body := ownedBackupMetadataBuffer{remaining: info.Size}
	n, err := s.blob.DownloadIfGenerationMatch(ctx, name, info.Generation, &body)
	if err != nil {
		return err
	}
	if n != info.Size {
		return fmt.Errorf("handoff backup metadata size changed for %s", name)
	}
	decoder := json.NewDecoder(&body.buffer)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("handoff backup metadata contains trailing data")
	}
	return nil
}

type ownedBackupMetadataBuffer struct {
	buffer    bytes.Buffer
	remaining int64
}

func (b *ownedBackupMetadataBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		return 0, errors.New("handoff backup metadata exceeds declared size")
	}
	b.remaining -= int64(len(p))
	return b.buffer.Write(p)
}
