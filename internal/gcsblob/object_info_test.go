package gcsblob

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestObservedObjectDownloadAndDeletion(t *testing.T) {
	srv := httptest.NewServer(newFakeGCS().handler())
	defer srv.Close()
	c := testClient(t, srv)
	ctx := context.Background()
	first, err := c.PutBytesIfGenerationMatch(ctx, "object", []byte("first"), 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Stat(ctx, "object")
	if err != nil || info.Name != "object" || info.Generation != first || info.Size != 5 {
		t.Fatalf("metadata=%+v error=%v", info, err)
	}
	second, err := c.PutBytesIfGenerationMatch(ctx, "object", []byte("second"), first)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := c.DownloadIfGenerationMatch(ctx, "object", first, &buf); !errors.Is(err, ErrPreconditionFailed) || buf.Len() != 0 {
		t.Fatalf("stale download wrote %d bytes: %v", buf.Len(), err)
	}
	if n, err := c.DownloadIfGenerationMatch(ctx, "object", second, &buf); err != nil || n != 6 || buf.String() != "second" {
		t.Fatalf("observed download bytes=%d body=%q error=%v", n, buf.String(), err)
	}
	if removed, err := c.DeleteIfGenerationMatch(ctx, "object", second); err != nil || !removed {
		t.Fatalf("confirmed deletion=%v error=%v", removed, err)
	}
	if removed, err := c.DeleteIfGenerationMatch(ctx, "object", second); err != nil || removed {
		t.Fatalf("absence counted as deletion=%v error=%v", removed, err)
	}
	if _, err := c.Stat(ctx, "object"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("deleted object metadata: %v", err)
	}
}
