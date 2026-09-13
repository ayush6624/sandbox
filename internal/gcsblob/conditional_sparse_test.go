package gcsblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConditionalSparseRoundtrip(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			srv := httptest.NewServer(newFakeGCS().handler())
			defer srv.Close()
			c := testClient(t, srv)
			ctx := context.Background()
			source, target := filepath.Join(t.TempDir(), "source"), filepath.Join(t.TempDir(), "target")
			raw := make([]byte, 128<<10)
			copy(raw[64<<10:], bytes.Repeat([]byte{31}, 4096))
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			first, err := c.PutBytesIfGenerationMatch(ctx, "object", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var next int64
			if partial {
				next, err = c.PutRangesIfGenerationMatch(ctx, "object", source, []Range{{Off: 64 << 10, Len: 4096}}, first)
			} else {
				next, err = c.PutSparseIfGenerationMatch(ctx, "object", source, first)
			}
			if err != nil || next == first {
				t.Fatalf("generation=%d error=%v", next, err)
			}
			if err := c.GetSparse(ctx, "object", target); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(raw, got) {
				t.Fatalf("roundtrip mismatch: %v", err)
			}
			if _, err := c.PutSparseIfGenerationMatch(ctx, "object", source, first); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("stale stream upload: %v", err)
			}
		})
	}
}

type pausedUploadBody struct {
	io.ReadCloser
	ctx       context.Context
	remaining int
	paused    chan struct{}
	resume    <-chan struct{}
	once      sync.Once
}

func (b *pausedUploadBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		b.once.Do(func() { close(b.paused) })
		select {
		case <-b.resume:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	} else if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	if b.remaining > 0 {
		b.remaining -= n
	}
	return n, err
}

type pauseUploadTransport struct {
	base       http.RoundTripper
	generation int64
	paused     chan struct{}
	resume     <-chan struct{}
}

func (tr pauseUploadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.URL.Query().Get("ifGenerationMatch") == strconv.FormatInt(tr.generation, 10) {
		req = req.Clone(req.Context())
		req.Body = &pausedUploadBody{ReadCloser: req.Body, ctx: req.Context(), remaining: 32 << 10, paused: tr.paused, resume: tr.resume}
	}
	return tr.base.RoundTrip(req)
}

func TestSparseUploadFenceDuringBodyTransfer(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	checkSparseUploadFence(t, testClient(t, srv))
}

func TestGCSSparseUploadFenceDuringBodyTransfer(t *testing.T) {
	bucket := os.Getenv("SANDBOX_GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("set SANDBOX_GCS_TEST_BUCKET to test a paused streaming upload against GCS")
	}
	checkSparseUploadFence(t, New(bucket))
}

func checkSparseUploadFence(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	object := "_verification/storage-write-fence/" + uuid.NewString() + "/stream"
	t.Logf("test object: gs://%s/%s", c.Bucket(), object)
	raw := make([]byte, 256<<10)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := c.PutBytesIfGenerationMatch(ctx, object, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, gen, err := c.GetBytesGen(cleanup, object)
		if errors.Is(err, ErrNotExist) {
			return
		}
		if err == nil {
			_, err = c.DeleteIfGenerationMatch(cleanup, object, gen)
		}
		if err != nil {
			t.Errorf("cleanup %s: %v", object, err)
		}
	}()
	paused, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	unpause := func() { resumeOnce.Do(func() { close(resume) }) }
	base := c.hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	original := c.hc
	c.hc = &http.Client{Transport: pauseUploadTransport{base, first, paused, resume}}
	defer func() { c.hc = original }()
	done := make(chan struct{})
	var uploadErr error
	go func() {
		defer close(done)
		_, uploadErr = c.PutSparseIfGenerationMatch(ctx, object, path, first)
	}()
	defer func() { unpause(); cancel(); <-done }()
	select {
	case <-paused:
	case <-done:
		t.Fatalf("upload finished before body pause: %v", uploadErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := c.DeleteIfGenerationMatch(ctx, object, first); err != nil {
		t.Fatal(err)
	}
	second, err := c.PutBytesIfGenerationMatch(ctx, object, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	unpause()
	<-done
	if !errors.Is(uploadErr, ErrPreconditionFailed) {
		t.Fatalf("stream finalized after placeholder replacement: %v", uploadErr)
	}
	data, generation, err := c.GetBytesGen(ctx, object)
	if err != nil || len(data) != 0 || generation != second {
		t.Fatalf("replacement changed: bytes=%d generation=%d error=%v", len(data), generation, err)
	}
	if _, err := c.DeleteIfGenerationMatch(ctx, object, second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetBytesGen(ctx, object); !errors.Is(err, ErrNotExist) {
		t.Fatalf("test key survived cleanup: %v", err)
	}
	t.Log("paused sparse upload rejected after placeholder replacement; test key removed")
}
