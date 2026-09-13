package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRawHandoffManifestHashesWithoutCompressionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem")
	page := bytes.Repeat([]byte{73}, 4096)
	data := append(append(append([]byte{}, page...), make([]byte, 4096)...), page...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := buildRawChunkManifest(context.Background(), path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 2 || m.Codec != "raw-sha256" || len(m.Chunks) != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Chunks[0].Hash != testChunkHash(page) || m.Chunks[1].Hash != chunkZeroHash || m.Chunks[2].Hash != m.Chunks[0].Hash {
		t.Fatalf("hashes = %+v", m.Chunks)
	}
	for _, entry := range m.Chunks {
		if entry.CLen != 0 {
			t.Fatal("foreground invented compressed length")
		}
	}
	m.Chunks[0].CLen = 1
	if err := m.validate(); err == nil {
		t.Fatal("raw manifest accepted compressed length")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildRawChunkManifest(ctx, path, 4096); err == nil {
		t.Fatal("canceled preparation kept scanning")
	}
}
