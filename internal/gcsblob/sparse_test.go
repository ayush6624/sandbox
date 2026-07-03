package gcsblob

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSparseRoundtrip exercises encodeSparse → decodeSparse: a file with data
// islands in a sea of holes must come back byte-identical, including the
// hole tail (size restored by truncate).
func TestSparseRoundtrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	const size = 8 << 20 // 8 MiB logical

	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		off  int64
		data []byte
	}{
		{0, bytes.Repeat([]byte{0xAA}, 4096)},
		{1 << 20, bytes.Repeat([]byte{0xBB}, 123456)},
		{size - 4096, bytes.Repeat([]byte{0xCC}, 4096)}, // data at EOF
	}
	for _, wr := range writes {
		if _, err := f.WriteAt(wr.data, wr.off); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	// Encode via the same path PutSparse uses.
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	ranges, err := dataRanges(sf)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := encodeSparse(&buf, sf, size, ranges); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeSparse(&buf, df); err != nil {
		t.Fatal(err)
	}
	df.Close()

	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("size mismatch: want %d got %d", len(want), len(got))
	}
	if !bytes.Equal(want, got) {
		t.Fatal("content mismatch after roundtrip")
	}
}

// TestSparseOverlay verifies that decoding a partial (diff-style) stream onto
// a pre-staged base only touches the encoded ranges — the base's other bytes
// survive. This is the rootfs base+overlay materialization path.
func TestSparseOverlay(t *testing.T) {
	dir := t.TempDir()
	const size = 1 << 20

	// Base: all 0x11. Diff source: 0x22 at one range.
	base := bytes.Repeat([]byte{0x11}, size)
	diffSrc := filepath.Join(dir, "diff")
	f, err := os.Create(diffSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	patch := bytes.Repeat([]byte{0x22}, 8192)
	const patchOff = 256 * 1024
	if _, err := f.WriteAt(patch, patchOff); err != nil {
		t.Fatal(err)
	}

	// Encode ONLY the patched range (as PutRanges would).
	var buf bytes.Buffer
	if err := encodeSparse(&buf, f, size, []Range{{Off: patchOff, Len: int64(len(patch))}}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Stage the base, then overlay.
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, base, 0o644); err != nil {
		t.Fatal(err)
	}
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeSparse(&buf, df); err != nil {
		t.Fatal(err)
	}
	df.Close()

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < size; i++ {
		want := byte(0x11)
		if i >= patchOff && i < patchOff+len(patch) {
			want = 0x22
		}
		if got[i] != want {
			t.Fatalf("byte %d: want %#x got %#x", i, want, got[i])
		}
	}
}
