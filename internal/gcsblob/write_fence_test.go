package gcsblob

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPlaceholderWriteFence(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	checkPlaceholderWriteFence(t, testClient(t, srv))
}

// This opt-in test writes only its fresh UUID key, then conditionally removes it.
// Run on GCE with the attached identity; no credentials are included in output.
func TestGCSPlaceholderWriteFence(t *testing.T) {
	bucket := os.Getenv("SANDBOX_GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("set SANDBOX_GCS_TEST_BUCKET to run the isolated GCS write-fence test")
	}
	checkPlaceholderWriteFence(t, New(bucket))
}

func checkPlaceholderWriteFence(t *testing.T, c *Client) {
	t.Helper()
	object := "_verification/storage-write-fence/" + uuid.NewString() + "/payload"
	t.Logf("test object: gs://%s/%s", c.Bucket(), object)
	payload := []byte("isolated storage write-fence verification")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	put := func(data []byte, gen int64) int64 {
		t.Helper()
		written, err := c.PutBytesIfGenerationMatch(ctx, object, data, gen)
		if err != nil {
			t.Fatalf("conditional upload: %v", err)
		}
		return written
	}
	remove := func(gen int64) {
		t.Helper()
		if _, err := c.DeleteIfGenerationMatch(ctx, object, gen); err != nil {
			t.Fatalf("conditional delete: %v", err)
		}
	}
	rejectPayload := func(gen int64) {
		t.Helper()
		if _, err := c.PutBytesIfGenerationMatch(ctx, object, payload, gen); !errors.Is(err, ErrPreconditionFailed) {
			t.Fatalf("stale payload upload error=%v, want precondition failure", err)
		}
	}
	first := put(nil, 0)
	remove(first)
	rejectPayload(first)
	rejectPayload(first)
	second := put(nil, 0)
	if first == second {
		t.Fatal("recreated placeholder reused the deleted generation")
	}
	rejectPayload(first)
	committed := put(payload, second)
	if committed == second {
		t.Fatal("payload reused its placeholder generation")
	}
	if _, err := c.DeleteIfGenerationMatch(ctx, object, second); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale deletion error=%v, want precondition failure", err)
	}
	data, gen, err := c.GetBytesGen(ctx, object)
	if err != nil || gen != committed || !bytes.Equal(data, payload) {
		t.Fatalf("stale delete changed committed payload: generation=%d error=%v", gen, err)
	}
	remove(committed)
	if _, _, err := c.GetBytesGen(ctx, object); !errors.Is(err, ErrNotExist) {
		t.Fatalf("test object remains after deletion: %v", err)
	}
	t.Logf("verified stale PUT/DELETE rejection and cleanup; generations %d, %d, %d", first, second, committed)
}

func TestConditionalDeleteRejectsNonpositiveGeneration(t *testing.T) {
	c := New("unused")
	for _, gen := range []int64{0, -1} {
		if _, err := c.DeleteIfGenerationMatch(context.Background(), "object", gen); err == nil {
			t.Fatalf("accepted generation %d", gen)
		}
	}
}
