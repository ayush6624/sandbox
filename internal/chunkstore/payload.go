package chunkstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ayush6624/sandbox/internal/gcsblob"
)

const maxPayloadAttempts = 64

var (
	ErrWriterClosed       = errors.New("chunk payload writer closed")
	ErrWriterOpen         = errors.New("chunk payload writer already open")
	ErrWriterSuspended    = errors.New("chunk payload writer attempt suspended")
	ErrInvalidPayloadName = errors.New("invalid chunk payload name")
	ErrEmptyPayload       = errors.New("chunk payload must be nonempty")
	ErrImmutablePayload   = errors.New("immutable chunk payload mismatch")
	ErrDeletedPayload     = errors.New("nonempty payload exists under a deleted generation")
	ErrRepeatedPageToken  = errors.New("chunk payload listing repeated a page token")
	ErrPayloadRetryLimit  = errors.New("chunk payload retry limit exceeded")
)

// PayloadBlob is the immutable payload subset implemented by gcsblob.Client.
// The catalog uses separate keys and only needs its smaller BlobStore subset.
type PayloadBlob interface {
	PutBytesIfGenerationMatch(context.Context, string, []byte, int64) (int64, error)
	PutSparseIfGenerationMatch(context.Context, string, string, int64) (int64, error)
	PutRangesIfGenerationMatch(context.Context, string, string, []gcsblob.Range, int64) (int64, error)
	Stat(context.Context, string) (gcsblob.ObjectInfo, error)
	DownloadIfGenerationMatch(context.Context, string, int64, io.Writer) (int64, error)
	ListObjects(context.Context, string, string) (gcsblob.ObjectPage, error)
	DeleteIfGenerationMatch(context.Context, string, int64) (bool, error)
}

var _ PayloadBlob = (*gcsblob.Client)(nil)

type Payloads struct {
	catalog *Store
	blob    PayloadBlob

	mu      sync.Mutex
	writers map[string]*Writer
}

func NewPayloads(catalog *Store, blob PayloadBlob) *Payloads {
	return &Payloads{catalog: catalog, blob: blob, writers: make(map[string]*Writer)}
}

// OpenWriter admits the publisher before it can issue any payload request.
// One Payloads instance permits one live handle for a publisher identity; the
// job coordinator must provide the same serialization across processes.
func (p *Payloads) OpenWriter(ctx context.Context, setID, publisherID string) (*Writer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := setID + "\x00" + publisherID
	if _, ok := p.writers[key]; ok {
		return nil, ErrWriterOpen
	}
	if err := p.catalog.Begin(ctx, setID, publisherID); err != nil {
		return nil, err
	}
	w := &Writer{payloads: p, setID: setID, publisherID: publisherID, registryKey: key}
	w.drained = sync.NewCond(&w.mu)
	p.writers[key] = w
	return w, nil
}

type Writer struct {
	payloads    *Payloads
	setID       string
	publisherID string
	registryKey string

	mu        sync.Mutex
	drained   *sync.Cond
	closing   bool
	inFlight  int
	released  bool
	suspended bool
}

// PayloadReceipt identifies the immutable object version accepted by a write.
// Its zero value does not identify a successful write.
type PayloadReceipt struct {
	name       string
	generation int64
}

func (r PayloadReceipt) Name() string      { return r.name }
func (r PayloadReceipt) Generation() int64 { return r.generation }

func (w *Writer) PutBytes(ctx context.Context, name string, data []byte) (PayloadReceipt, error) {
	data = bytes.Clone(data)
	return w.write(func() (PayloadReceipt, error) {
		if len(data) == 0 {
			return PayloadReceipt{}, ErrEmptyPayload
		}
		return w.putImmutable(ctx, name,
			func(ctx context.Context, object string, generation int64) (int64, error) {
				return w.payloads.blob.PutBytesIfGenerationMatch(ctx, object, data, generation)
			},
			func(dst io.Writer) error {
				_, err := dst.Write(data)
				return err
			},
		)
	})
}

func (w *Writer) PutSparse(ctx context.Context, name, sourcePath string) (PayloadReceipt, error) {
	return w.write(func() (PayloadReceipt, error) {
		return w.putImmutable(ctx, name,
			func(ctx context.Context, object string, generation int64) (int64, error) {
				return w.payloads.blob.PutSparseIfGenerationMatch(ctx, object, sourcePath, generation)
			},
			func(dst io.Writer) error {
				_, err := gcsblob.WriteSparse(dst, sourcePath)
				return err
			},
		)
	})
}

func (w *Writer) PutRanges(ctx context.Context, name, sourcePath string, ranges []gcsblob.Range) (PayloadReceipt, error) {
	ranges = append([]gcsblob.Range(nil), ranges...)
	return w.write(func() (PayloadReceipt, error) {
		return w.putImmutable(ctx, name,
			func(ctx context.Context, object string, generation int64) (int64, error) {
				return w.payloads.blob.PutRangesIfGenerationMatch(ctx, object, sourcePath, ranges, generation)
			},
			func(dst io.Writer) error {
				_, err := gcsblob.WriteRanges(dst, sourcePath, ranges)
				return err
			},
		)
	})
}

func (w *Writer) write(fn func() (PayloadReceipt, error)) (PayloadReceipt, error) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return PayloadReceipt{}, ErrWriterClosed
	}
	w.inFlight++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.inFlight--
		if w.inFlight == 0 {
			w.drained.Broadcast()
		}
		w.mu.Unlock()
	}()
	return fn()
}

// Close rejects new writes and waits for every admitted operation to return.
// The caller must attach any durable root before Close; Close does not publish
// one. A canceled release leaves the publisher in the catalog for a later Close.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closing = true
	for w.inFlight != 0 {
		w.drained.Wait()
	}
	if w.suspended {
		return ErrWriterSuspended
	}
	if w.released {
		return nil
	}
	if err := w.payloads.catalog.ReleasePublisher(ctx, w.setID, w.publisherID); err != nil {
		return err
	}
	w.released = true
	w.payloads.mu.Lock()
	delete(w.payloads.writers, w.registryKey)
	w.payloads.mu.Unlock()
	return nil
}

// Suspend ends this local attempt without releasing its durable publisher.
// The job coordinator must serialize attempts for one publisher identity; a
// later OpenWriter can then resume the same generation after a failed backup.
func (w *Writer) Suspend(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.released {
		return ErrWriterClosed
	}
	w.closing = true
	for w.inFlight != 0 {
		w.drained.Wait()
	}
	if !w.suspended {
		w.suspended = true
		w.payloads.mu.Lock()
		if w.payloads.writers[w.registryKey] == w {
			delete(w.payloads.writers, w.registryKey)
		}
		w.payloads.mu.Unlock()
	}
	return ctx.Err()
}

type payloadUpload func(context.Context, string, int64) (int64, error)
type payloadEncoding func(io.Writer) error

func (w *Writer) putImmutable(ctx context.Context, name string, upload payloadUpload, encode payloadEncoding) (PayloadReceipt, error) {
	if err := validatePayloadName(w.setID, name); err != nil {
		return PayloadReceipt{}, err
	}
	object := payloadKey(w.setID, name)
	var placeholder int64
	for attempt := 0; attempt < maxPayloadAttempts; attempt++ {
		var operationErr error
		uploadAttempted := false
		if err := ctx.Err(); err != nil {
			return PayloadReceipt{}, err
		}
		if placeholder == 0 {
			generation, err := w.payloads.blob.PutBytesIfGenerationMatch(ctx, object, nil, 0)
			switch {
			case err == nil:
				if generation <= 0 {
					return PayloadReceipt{}, errors.New("payload placeholder has nonpositive generation")
				}
				placeholder = generation
			case errors.Is(err, gcsblob.ErrPreconditionFailed):
			default:
				if ctx.Err() != nil {
					return PayloadReceipt{}, ctx.Err()
				}
				operationErr = err
			}
		}
		if placeholder > 0 {
			uploadAttempted = true
			generation, err := upload(ctx, object, placeholder)
			if err == nil {
				if generation <= 0 {
					return PayloadReceipt{}, errors.New("payload upload has nonpositive generation")
				}
				return PayloadReceipt{name: name, generation: generation}, nil
			}
			if ctx.Err() != nil {
				return PayloadReceipt{}, ctx.Err()
			}
			operationErr = err
		}

		info, err := w.payloads.blob.Stat(ctx, object)
		if errors.Is(err, gcsblob.ErrNotExist) {
			if operationErr != nil && !errors.Is(operationErr, gcsblob.ErrPreconditionFailed) {
				return PayloadReceipt{}, operationErr
			}
			placeholder = 0
			continue
		}
		if err != nil {
			return PayloadReceipt{}, err
		}
		if err := validateObjectInfo(info, object); err != nil {
			return PayloadReceipt{}, err
		}
		if info.Size == 0 {
			if uploadAttempted && operationErr != nil && !errors.Is(operationErr, gcsblob.ErrPreconditionFailed) {
				return PayloadReceipt{}, operationErr
			}
			placeholder = info.Generation
			continue
		}
		same, err := w.matches(ctx, object, info, encode)
		if errors.Is(err, gcsblob.ErrPreconditionFailed) || errors.Is(err, gcsblob.ErrNotExist) {
			placeholder = 0
			continue
		}
		if err != nil {
			return PayloadReceipt{}, err
		}
		if same {
			return PayloadReceipt{name: name, generation: info.Generation}, nil
		}
		return PayloadReceipt{}, fmt.Errorf("%w: %s", ErrImmutablePayload, name)
	}
	return PayloadReceipt{}, ErrPayloadRetryLimit
}

func (w *Writer) matches(ctx context.Context, object string, info gcsblob.ObjectInfo, encode payloadEncoding) (bool, error) {
	expected := sha256.New()
	if err := encode(expected); err != nil {
		return false, err
	}
	actual := sha256.New()
	n, err := w.payloads.blob.DownloadIfGenerationMatch(ctx, object, info.Generation, actual)
	if err != nil {
		return false, err
	}
	if n != info.Size {
		return false, fmt.Errorf("conditional download size %d does not match metadata %d", n, info.Size)
	}
	return bytes.Equal(expected.Sum(nil), actual.Sum(nil)), nil
}

type Collection struct {
	Objects int
	Bytes   int64
}

// Collect reclaims one retired generation prefix. Any uncertain list, stat,
// or delete leaves the durable catalog in Deleting for an idempotent retry.
func (p *Payloads) Collect(ctx context.Context, setID string) (Collection, error) {
	if err := p.catalog.MarkDeleting(ctx, setID); err != nil {
		return Collection{}, err
	}
	view, err := p.catalog.Inspect(ctx, setID)
	if err != nil {
		return Collection{}, err
	}
	deleted := view.Phase == Deleted
	prefix := DataPrefix(setID)
	var result Collection
	for sweep := 0; sweep < maxPayloadAttempts; sweep++ {
		objects, err := p.listPayloads(ctx, prefix)
		if err != nil {
			return result, err
		}
		if len(objects) == 0 {
			if !deleted {
				if err := p.catalog.MarkDeleted(ctx, setID); err != nil {
					return result, err
				}
			}
			return result, nil
		}
		if deleted {
			for _, object := range objects {
				if object.Size != 0 {
					return result, fmt.Errorf("%w: %s", ErrDeletedPayload, object.Name)
				}
			}
		}
		for _, object := range objects {
			removed, size, err := p.deletePayload(ctx, prefix, object, deleted)
			if err != nil {
				return result, err
			}
			if removed {
				result.Objects++
				result.Bytes += size
			}
		}
	}
	return result, ErrPayloadRetryLimit
}

func (p *Payloads) listPayloads(ctx context.Context, prefix string) ([]gcsblob.ObjectInfo, error) {
	var objects []gcsblob.ObjectInfo
	seen := map[string]bool{}
	for token := ""; ; {
		page, err := p.blob.ListObjects(ctx, prefix, token)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Objects {
			if err := validateObjectInfo(object, object.Name); err != nil {
				return nil, err
			}
			if !strings.HasPrefix(object.Name, prefix) {
				return nil, fmt.Errorf("listed payload %q is outside %q", object.Name, prefix)
			}
			objects = append(objects, object)
		}
		next := page.NextPageToken
		if next == "" {
			return objects, nil
		}
		if next == token || seen[next] {
			return nil, ErrRepeatedPageToken
		}
		seen[next] = true
		token = next
	}
}

func (p *Payloads) deletePayload(ctx context.Context, prefix string, info gcsblob.ObjectInfo, deleted bool) (bool, int64, error) {
	for attempt := 0; attempt < maxPayloadAttempts; attempt++ {
		if !strings.HasPrefix(info.Name, prefix) {
			return false, 0, fmt.Errorf("payload %q escaped prefix %q", info.Name, prefix)
		}
		if deleted && info.Size != 0 {
			return false, 0, fmt.Errorf("%w: %s", ErrDeletedPayload, info.Name)
		}
		removed, err := p.blob.DeleteIfGenerationMatch(ctx, info.Name, info.Generation)
		if err == nil {
			if removed {
				return true, info.Size, nil
			}
			return false, 0, nil
		}
		if !errors.Is(err, gcsblob.ErrPreconditionFailed) {
			return false, 0, err
		}
		latest, err := p.blob.Stat(ctx, info.Name)
		if errors.Is(err, gcsblob.ErrNotExist) {
			return false, 0, nil
		}
		if err != nil {
			return false, 0, err
		}
		if err := validateObjectInfo(latest, info.Name); err != nil {
			return false, 0, err
		}
		info = latest
	}
	return false, 0, ErrPayloadRetryLimit
}

// DataPrefix returns the immutable data namespace for an already-validated,
// canonical generation set ID. Public boundary methods validate before use.
func DataPrefix(setID string) string { return "chunksets/data/" + setID + "/" }

func payloadKey(setID, name string) string { return DataPrefix(setID) + name }

func validatePayloadName(setID, name string) error {
	if name == "" || !utf8.ValidString(name) || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.Contains(name, "//") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return ErrInvalidPayloadName
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." {
			return ErrInvalidPayloadName
		}
	}
	if len(payloadKey(setID, name)) > 1024 {
		return ErrInvalidPayloadName
	}
	return nil
}

func validateObjectInfo(info gcsblob.ObjectInfo, wantName string) error {
	if info.Name != wantName || info.Generation <= 0 || info.Size < 0 {
		return fmt.Errorf("invalid object metadata for %q", wantName)
	}
	return nil
}
