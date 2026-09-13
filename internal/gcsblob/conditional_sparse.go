package gcsblob

import (
	"context"
	"fmt"
	"os"
)

// PutSparseIfGenerationMatch replaces an existing placeholder with a sparse
// stream. Every retry keeps the captured positive generation. Returns the new
// object generation, not the uncompressed file size.
func (c *Client) PutSparseIfGenerationMatch(ctx context.Context, object, path string, generation int64) (int64, error) {
	if generation <= 0 {
		return 0, fmt.Errorf("conditional sparse upload requires a positive generation")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	ranges, err := dataRanges(f)
	if err != nil {
		return 0, fmt.Errorf("enumerate data ranges of %s: %w", path, err)
	}
	_, next, err := c.putRanges(ctx, object, f, ranges, &generation)
	return next, err
}

// PutRangesIfGenerationMatch applies the same fence to a sparse diff stream.
func (c *Client) PutRangesIfGenerationMatch(ctx context.Context, object, path string, ranges []Range, generation int64) (int64, error) {
	if generation <= 0 {
		return 0, fmt.Errorf("conditional range upload requires a positive generation")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, next, err := c.putRanges(ctx, object, f, ranges, &generation)
	return next, err
}
