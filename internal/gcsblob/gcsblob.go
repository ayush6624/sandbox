// Package gcsblob is a minimal GCS client for snapshot durability: it uploads
// and downloads snapshot artifacts as gzipped sparse streams, authenticated via
// the GCE metadata server (the VM's attached service account). Hand-rolled over
// the official SDK to keep the dependency tree pure-Go and small — we need
// exactly four verbs (put/get/exists/delete) against one bucket.
//
// Wire format for file objects ("sparse stream"): gzip-compressed
//
//	[8]byte  magic "SBSPARSE"
//	uint64   version (1)
//	uint64   original file size
//	repeated frames until EOF:
//	  uint64 offset
//	  uint64 length
//	  [length]byte payload
//
// Only data regions are encoded (holes and, for diff uploads, unchanged
// ranges are skipped), so a 10 GB mostly-hole file costs what its data costs.
// Decoding truncates the target to the original size and pwrites each frame,
// which composes with a pre-staged base copy: reflink the base into place,
// then overlay a diff stream on top.
package gcsblob

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// ErrNotExist is returned by Get*/Exists paths when the object is absent.
// It wraps fs.ErrNotExist so errors.Is(err, fs.ErrNotExist) also matches.
var ErrNotExist = fmt.Errorf("gcs object: %w", fs.ErrNotExist)

const (
	sparseMagic   = "SBSPARSE"
	sparseVersion = 1

	metadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	storageBase      = "https://storage.googleapis.com/storage/v1"
	uploadBase       = "https://storage.googleapis.com/upload/storage/v1"

	putAttempts = 3
)

// Range is a [Off, Off+Len) byte range of a file to include in a diff upload.
type Range struct {
	Off int64
	Len int64
}

// Client talks to a single GCS bucket.
type Client struct {
	bucket string
	hc     *http.Client

	mu     sync.Mutex
	tok    string
	tokExp time.Time
}

// New returns a client for bucket. No network I/O happens until first use.
func New(bucket string) *Client {
	return &Client{bucket: bucket, hc: &http.Client{}}
}

// Bucket returns the bucket this client targets.
func (c *Client) Bucket() string { return c.bucket }

// token returns a cached service-account access token from the GCE metadata
// server, refreshing when within a minute of expiry.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && time.Now().Before(c.tokExp.Add(-time.Minute)) {
		return c.tok, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", metadataTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token (no service account attached?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("metadata token: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode metadata token: %w", err)
	}
	c.tok = tok.AccessToken
	c.tokExp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.tok, nil
}

func (c *Client) newReq(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req, nil
}

func (c *Client) objectURL(object string) string {
	return fmt.Sprintf("%s/b/%s/o/%s", storageBase, url.PathEscape(c.bucket), url.PathEscape(object))
}

// PutBytes uploads data as object, overwriting any existing object.
func (c *Client) PutBytes(ctx context.Context, object string, data []byte) error {
	return c.retry(ctx, func() error {
		u := fmt.Sprintf("%s/b/%s/o?uploadType=media&name=%s",
			uploadBase, url.PathEscape(c.bucket), url.QueryEscape(object))
		req, err := c.newReq(ctx, "POST", u, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer drainClose(resp)
		if resp.StatusCode != 200 {
			return fmt.Errorf("upload %s: HTTP %d", object, resp.StatusCode)
		}
		return nil
	})
}

// GetBytes downloads object. Returns ErrNotExist for a missing object.
func (c *Client) GetBytes(ctx context.Context, object string) ([]byte, error) {
	req, err := c.newReq(ctx, "GET", c.objectURL(object)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp)
	switch resp.StatusCode {
	case 200:
		return io.ReadAll(resp.Body)
	case 404:
		return nil, ErrNotExist
	default:
		return nil, fmt.Errorf("download %s: HTTP %d", object, resp.StatusCode)
	}
}

// Exists reports whether object exists (a metadata GET, no payload transfer).
func (c *Client) Exists(ctx context.Context, object string) (bool, error) {
	req, err := c.newReq(ctx, "GET", c.objectURL(object), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer drainClose(resp)
	switch resp.StatusCode {
	case 200:
		return true, nil
	case 404:
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: HTTP %d", object, resp.StatusCode)
	}
}

// Delete removes object; a missing object is not an error.
func (c *Client) Delete(ctx context.Context, object string) error {
	req, err := c.newReq(ctx, "DELETE", c.objectURL(object), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp)
	if resp.StatusCode != 204 && resp.StatusCode != 404 {
		return fmt.Errorf("delete %s: HTTP %d", object, resp.StatusCode)
	}
	return nil
}

// PutSparse uploads path as a sparse stream, encoding only its data regions
// (holes are skipped via SEEK_DATA/SEEK_HOLE). Returns the number of payload
// bytes encoded (pre-compression), for logging.
func (c *Client) PutSparse(ctx context.Context, object, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	ranges, err := dataRanges(f)
	if err != nil {
		return 0, fmt.Errorf("enumerate data ranges of %s: %w", path, err)
	}
	return c.putRanges(ctx, object, f, ranges)
}

// PutRanges uploads only the given ranges of path as a sparse stream — the
// diff-upload variant, with ranges supplied by an extent comparison instead of
// the file's own hole map. Returns payload bytes encoded.
func (c *Client) PutRanges(ctx context.Context, object, path string, ranges []Range) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return c.putRanges(ctx, object, f, ranges)
}

func (c *Client) putRanges(ctx context.Context, object string, f *os.File, ranges []Range) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var payload int64
	for _, r := range ranges {
		payload += r.Len
	}

	err = c.retry(ctx, func() error {
		pr, pw := io.Pipe()
		// Encoder goroutine: frames → gzip → pipe → HTTP body.
		go func() {
			pw.CloseWithError(encodeSparse(pw, f, fi.Size(), ranges))
		}()
		u := fmt.Sprintf("%s/b/%s/o?uploadType=media&name=%s",
			uploadBase, url.PathEscape(c.bucket), url.QueryEscape(object))
		req, err := c.newReq(ctx, "POST", u, pr)
		if err != nil {
			pr.Close()
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer drainClose(resp)
		if resp.StatusCode != 200 {
			return fmt.Errorf("upload %s: HTTP %d", object, resp.StatusCode)
		}
		return nil
	})
	return payload, err
}

// GetSparse downloads a sparse-stream object into path: the file is created if
// needed, truncated to the stream's original size, and each frame is written
// at its offset. Pre-staging path with a base copy before calling composes a
// diff onto that base. Returns ErrNotExist for a missing object.
func (c *Client) GetSparse(ctx context.Context, object, path string) error {
	req, err := c.newReq(ctx, "GET", c.objectURL(object)+"?alt=media", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp)
	switch resp.StatusCode {
	case 200:
	case 404:
		return ErrNotExist
	default:
		return fmt.Errorf("download %s: HTTP %d", object, resp.StatusCode)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := decodeSparse(resp.Body, f); err != nil {
		return fmt.Errorf("decode %s into %s: %w", object, path, err)
	}
	return f.Sync()
}

// encodeSparse writes the sparse-stream format for the given ranges of f to w.
func encodeSparse(w io.Writer, f *os.File, fileSize int64, ranges []Range) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	hdr := make([]byte, 8+8+8)
	copy(hdr, sparseMagic)
	binary.LittleEndian.PutUint64(hdr[8:], sparseVersion)
	binary.LittleEndian.PutUint64(hdr[16:], uint64(fileSize))
	if _, err := zw.Write(hdr); err != nil {
		return err
	}
	frame := make([]byte, 16)
	for _, r := range ranges {
		if r.Len <= 0 {
			continue
		}
		binary.LittleEndian.PutUint64(frame, uint64(r.Off))
		binary.LittleEndian.PutUint64(frame[8:], uint64(r.Len))
		if _, err := zw.Write(frame); err != nil {
			return err
		}
		if _, err := io.Copy(zw, io.NewSectionReader(f, r.Off, r.Len)); err != nil {
			return fmt.Errorf("read range @%d+%d: %w", r.Off, r.Len, err)
		}
	}
	return zw.Close()
}

// decodeSparse reads a sparse stream from r and applies it to f.
func decodeSparse(r io.Reader, f *os.File) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	hdr := make([]byte, 8+8+8)
	if _, err := io.ReadFull(zr, hdr); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	if string(hdr[:8]) != sparseMagic {
		return errors.New("bad magic (not a sparse stream)")
	}
	if v := binary.LittleEndian.Uint64(hdr[8:]); v != sparseVersion {
		return fmt.Errorf("unsupported sparse stream version %d", v)
	}
	fileSize := int64(binary.LittleEndian.Uint64(hdr[16:]))
	if err := f.Truncate(fileSize); err != nil {
		return err
	}
	frame := make([]byte, 16)
	buf := make([]byte, 1<<20)
	for {
		if _, err := io.ReadFull(zr, frame); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // clean end of stream
			}
			return fmt.Errorf("read frame header: %w", err)
		}
		off := int64(binary.LittleEndian.Uint64(frame))
		length := int64(binary.LittleEndian.Uint64(frame[8:]))
		if off < 0 || length <= 0 || off+length > fileSize {
			return fmt.Errorf("frame @%d+%d out of bounds (file size %d)", off, length, fileSize)
		}
		for length > 0 {
			n := int64(len(buf))
			if length < n {
				n = length
			}
			if _, err := io.ReadFull(zr, buf[:n]); err != nil {
				return fmt.Errorf("read frame payload: %w", err)
			}
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				return err
			}
			off += n
			length -= n
		}
	}
}

// retry runs op up to putAttempts times with a short backoff. Uploads restart
// their stream on each attempt (the encoder re-reads from the source file).
func (c *Client) retry(ctx context.Context, op func() error) error {
	var err error
	for i := 0; i < putAttempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	return err
}

// drainClose fully drains and closes a response body so the connection is
// reusable.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
