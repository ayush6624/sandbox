package chunkstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
)

type receiptTestBlob struct {
	*payloadTestBlob
	afterUpload        func(string, int64) error
	beforeDownload     func(string)
	afterDownload      func(string)
	uploadGeneration   int64
	downloadGeneration int64
	statCalls          int
}

func (b *receiptTestBlob) PutBytesIfGenerationMatch(ctx context.Context, key string, data []byte, want int64) (int64, error) {
	generation, err := b.payloadTestBlob.PutBytesIfGenerationMatch(ctx, key, data, want)
	if err == nil && len(data) > 0 {
		b.uploadGeneration = generation
		if hook := b.afterUpload; hook != nil {
			b.afterUpload = nil
			if err := hook(key, generation); err != nil {
				return 0, err
			}
		}
	}
	return generation, err
}
func (b *receiptTestBlob) Stat(ctx context.Context, key string) (gcsblob.ObjectInfo, error) {
	b.statCalls++
	return b.payloadTestBlob.Stat(ctx, key)
}
func (b *receiptTestBlob) DownloadIfGenerationMatch(ctx context.Context, key string, want int64, dst io.Writer) (int64, error) {
	if hook := b.beforeDownload; hook != nil {
		b.beforeDownload = nil
		hook(key)
	}
	n, err := b.payloadTestBlob.DownloadIfGenerationMatch(ctx, key, want, dst)
	if err == nil {
		b.downloadGeneration = want
		if hook := b.afterDownload; hook != nil {
			b.afterDownload = nil
			hook(key)
		}
	}
	return n, err
}

func TestPayloadReceiptIdentifiesAcceptedGeneration(t *testing.T) {
	for _, mode := range []string{"upload", "immutable retry", "lost placeholder acknowledgment", "lost upload acknowledgment", "replacement during comparison"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			blob := &receiptTestBlob{payloadTestBlob: newPayloadTestBlob()}
			payloads := NewPayloads(New(blob), blob)
			w, err := payloads.OpenWriter(ctx, testSet, "publisher")
			must(t, err)
			t.Cleanup(func() { must(t, w.Suspend(ctx)) })
			key := payloadKey(testSet, "state.sz")
			switch mode {
			case "upload":
				blob.afterUpload = func(key string, _ int64) error { blob.replace(key, []byte("different")); return nil }
			case "immutable retry":
				blob.replace(key, []byte("original"))
				blob.afterDownload = func(key string) { blob.replace(key, []byte("different")) }
			case "lost placeholder acknowledgment":
				blob.afterPut = func() error { return io.ErrUnexpectedEOF }
			case "lost upload acknowledgment":
				blob.afterUpload = func(string, int64) error { return io.ErrUnexpectedEOF }
			case "replacement during comparison":
				blob.replace(key, []byte("original"))
				blob.beforeDownload = func(key string) { blob.replace(key, []byte("original")) }
			}
			receipt, err := w.PutBytes(ctx, "state.sz", []byte("original"))
			must(t, err)
			want := blob.downloadGeneration
			if mode == "lost placeholder acknowledgment" {
				want = blob.uploadGeneration
			}
			if mode == "upload" {
				want = blob.uploadGeneration
				if blob.statCalls != 0 {
					t.Fatalf("successful upload made %d extra metadata reads", blob.statCalls)
				}
			}
			if receipt.Name() != "state.sz" || receipt.Generation() != want || want <= 0 {
				t.Fatalf("receipt=%+v accepted generation=%d", receipt, want)
			}
			if mode == "upload" || mode == "immutable retry" {
				info, err := blob.payloadTestBlob.Stat(ctx, key)
				must(t, err)
				if receipt.Generation() == info.Generation {
					t.Fatal("receipt adopted unverified replacement generation")
				}
			}
		})
	}
}

func TestPayloadReceiptsPreserveSparseAndRangeRetryIdentity(t *testing.T) {
	ctx := context.Background()
	payloads, _, blob := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	t.Cleanup(func() { must(t, w.Suspend(ctx)) })
	path := filepath.Join(t.TempDir(), "source")
	must(t, os.WriteFile(path, []byte("source contents"), 0600))
	for _, ranges := range []bool{false, true} {
		write := func() (PayloadReceipt, error) {
			if ranges {
				return w.PutRanges(ctx, "rootfs.sz", path, []gcsblob.Range{{Off: 0, Len: 6}})
			}
			return w.PutSparse(ctx, "state.sz", path)
		}
		first, err := write()
		must(t, err)
		again, err := write()
		must(t, err)
		if first != again || first.Generation() <= 0 {
			t.Fatalf("retry changed receipt: %+v -> %+v", first, again)
		}
		info, err := blob.Stat(ctx, payloadKey(testSet, first.Name()))
		must(t, err)
		if first.Generation() != info.Generation {
			t.Fatalf("receipt=%+v stored=%+v", first, info)
		}
	}
}

func TestPayloadWriteFailuresReturnNoReceipt(t *testing.T) {
	ctx := context.Background()
	payloads, _, _ := payloadFixture()
	w, err := payloads.OpenWriter(ctx, testSet, "publisher")
	must(t, err)
	_, err = w.PutBytes(ctx, "payload", []byte("original"))
	must(t, err)
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"payload", []byte("different"), ErrImmutablePayload},
		{"../invalid", []byte("bytes"), ErrInvalidPayloadName},
		{"empty", nil, ErrEmptyPayload},
	} {
		receipt, err := w.PutBytes(ctx, tc.name, tc.data)
		if !errors.Is(err, tc.want) || receipt != (PayloadReceipt{}) {
			t.Fatalf("receipt=%+v err=%v want=%v", receipt, err, tc.want)
		}
	}
	must(t, w.Suspend(ctx))
	receipt, err := w.PutBytes(ctx, "late", []byte("late"))
	if !errors.Is(err, ErrWriterClosed) || receipt != (PayloadReceipt{}) {
		t.Fatalf("closed receipt=%+v err=%v", receipt, err)
	}
}
