// Package chunkstore tracks ownership of immutable checkpoint generations.
// It does not establish upload completion, consumer death, or permission to
// retire a durable root; lifecycle callers must establish those facts first.
package chunkstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

const (
	MaxActiveHolders = 4096
	MaxCatalogBytes  = 1 << 20
	maxCASAttempts   = 64
)

var (
	ErrInvalidID         = errors.New("invalid chunk storage identity")
	ErrInvalidCatalog    = errors.New("invalid chunk storage catalog")
	ErrNotFound          = errors.New("chunk storage catalog not found")
	ErrHolderNotFound    = errors.New("chunk storage holder not found")
	ErrIdentityConflict  = errors.New("chunk storage identity already has another role")
	ErrReleased          = errors.New("chunk storage holder already released")
	ErrRetired           = errors.New("chunk storage generation retired")
	ErrInvalidTransition = errors.New("invalid chunk storage lifecycle transition")
	ErrCapacity          = errors.New("chunk storage catalog capacity exceeded")
	ErrRetryLimit        = errors.New("chunk storage catalog CAS retry limit exceeded")
)

type Phase string

const (
	Publishing Phase = "publishing"
	Live       Phase = "live"
	Retired    Phase = "retired"
	Deleting   Phase = "deleting"
	Deleted    Phase = "deleted"
)

// BlobStore is the generation-CAS subset implemented by gcsblob.Client.
type BlobStore interface {
	GetBytesGen(context.Context, string) ([]byte, int64, error)
	PutBytesIfGenerationMatch(context.Context, string, []byte, int64) (int64, error)
}

var _ BlobStore = (*gcsblob.Client)(nil)

type Store struct{ blob BlobStore }

func New(blob BlobStore) *Store { return &Store{blob: blob} }

// View is an independent copy of the ownership state, including root receipts.
type View struct {
	SetID     string
	Phase     Phase
	Publisher PublisherView
	Roots     []RootView
	Readers   []ReaderView
}

type PublisherView struct {
	ID       string
	Released bool
}

type RootView struct {
	ID      string
	Retired bool
}

type ReaderView struct {
	ID      string
	RootID  string
	OwnerID string
}

type publisher struct {
	ID       string `json:"id"`
	Released bool   `json:"released"`
}

type root struct {
	Retired bool `json:"retired"`
}

type reader struct {
	RootID  string `json:"root_id"`
	OwnerID string `json:"owner_id"`
}

type catalog struct {
	Version   int               `json:"version"`
	SetID     string            `json:"set_id"`
	Phase     Phase             `json:"phase"`
	Publisher publisher         `json:"publisher"`
	Roots     map[string]root   `json:"roots"`
	Readers   map[string]reader `json:"readers"`
}

func catalogKey(setID string) string { return "chunksets/catalog/" + setID + ".json" }

// Begin creates the generation before any payload request. Replaying Begin
// cannot add a second publisher or revive the original publisher after release.
func (s *Store) Begin(ctx context.Context, setID, publisherID string) error {
	if err := validateInputs(setID, publisherID); err != nil {
		return err
	}
	return s.update(ctx, setID, publisherID, func(c *catalog) (bool, error) {
		if c.Publisher.ID != publisherID {
			return false, ErrIdentityConflict
		}
		if err := c.requireOpen(); err != nil {
			return false, err
		}
		if c.Publisher.Released {
			return false, ErrReleased
		}
		return false, nil
	})
}

// AttachRoot precedes publication of the durable root. Retired root identities
// remain as receipts so a delayed attachment cannot resurrect the same root.
func (s *Store) AttachRoot(ctx context.Context, setID, publisherID, rootID string) error {
	if err := validateInputs(setID, publisherID, rootID); err != nil {
		return err
	}
	return s.update(ctx, setID, "", func(c *catalog) (bool, error) {
		if c.Publisher.ID != publisherID {
			return false, ErrIdentityConflict
		}
		if err := c.requireOpen(); err != nil {
			return false, err
		}
		if c.Publisher.Released {
			return false, ErrReleased
		}
		if rootID == c.Publisher.ID {
			return false, ErrIdentityConflict
		}
		if _, ok := c.Readers[rootID]; ok {
			return false, ErrIdentityConflict
		}
		if r, ok := c.Roots[rootID]; ok {
			if r.Retired {
				return false, ErrReleased
			}
			return false, nil
		}
		c.Roots[rootID] = root{}
		c.Phase = Live
		return true, nil
	})
}

// ReaderIdentity is the immutable obligation persisted before acquisition.
type ReaderIdentity struct {
	SetID    string
	RootID   string
	OwnerID  string
	ReaderID string
}

type readerState uint8

const (
	readerPrepared readerState = iota
	readerReleaseOnly
	readerClosed
)

// Reader can attempt acquisition once. Its identity remains available after any
// error so the caller can retain and eventually release the durable obligation.
type Reader struct {
	store    *Store
	identity ReaderIdentity
	mu       sync.Mutex
	state    readerState
}

// PrepareReader reserves an identity without performing remote I/O. The caller
// must persist Identity before calling Acquire.
func (s *Store) PrepareReader(setID, rootID, ownerID string) (*Reader, error) {
	if err := validateInputs(setID, rootID); err != nil {
		return nil, err
	}
	if err := validateUUID(ownerID); err != nil {
		return nil, err
	}
	return &Reader{store: s, identity: ReaderIdentity{
		SetID: setID, RootID: rootID, OwnerID: ownerID, ReaderID: uuid.NewString(),
	}}, nil
}

// RecoverReader reconstructs a close-only obligation. The caller must prove
// owner death or consumer drain before using it to remove remote protection.
func (s *Store) RecoverReader(identity ReaderIdentity) (*Reader, error) {
	if err := validateInputs(identity.SetID, identity.RootID); err != nil {
		return nil, err
	}
	if err := validateUUID(identity.OwnerID); err != nil {
		return nil, err
	}
	if err := validateUUID(identity.ReaderID); err != nil {
		return nil, err
	}
	return &Reader{store: s, identity: identity, state: readerReleaseOnly}, nil
}

func (r *Reader) Identity() ReaderIdentity { return r.identity }

// Acquire retains a root's generation independently of root retirement. Every
// return disables further acquisition, including an ambiguous failure.
func (r *Reader) Acquire(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != readerPrepared {
		return ErrInvalidTransition
	}
	r.state = readerReleaseOnly
	identity := r.identity
	want := reader{RootID: identity.RootID, OwnerID: identity.OwnerID}
	return r.store.update(ctx, identity.SetID, "", func(c *catalog) (bool, error) {
		if existing, ok := c.Readers[identity.ReaderID]; ok {
			if existing != want {
				return false, ErrIdentityConflict
			}
			return false, nil
		}
		if identity.ReaderID == c.Publisher.ID {
			return false, ErrIdentityConflict
		}
		if _, ok := c.Roots[identity.ReaderID]; ok {
			return false, ErrIdentityConflict
		}
		if err := c.requireOpen(); err != nil {
			return false, err
		}
		if c.Phase != Live {
			return false, ErrInvalidTransition
		}
		if identity.RootID == c.Publisher.ID {
			return false, ErrIdentityConflict
		}
		if _, ok := c.Readers[identity.RootID]; ok {
			return false, ErrIdentityConflict
		}
		r, ok := c.Roots[identity.RootID]
		if !ok {
			return false, ErrHolderNotFound
		}
		if r.Retired {
			return false, ErrReleased
		}
		c.Readers[identity.ReaderID] = want
		return true, nil
	})
}

// Close follows consumer drain or authoritative owner death. Its successful CAS
// fences delayed acquisition PUTs even when the reader is already absent. Failed
// closes remain retryable; acquisition is disabled before any remote request.
func (r *Reader) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == readerClosed {
		return nil
	}
	r.state = readerReleaseOnly
	identity := r.identity
	want := reader{RootID: identity.RootID, OwnerID: identity.OwnerID}
	err := r.store.update(ctx, identity.SetID, "", func(c *catalog) (bool, error) {
		if existing, ok := c.Readers[identity.ReaderID]; ok && existing != want {
			return false, ErrIdentityConflict
		}
		if identity.ReaderID == c.Publisher.ID {
			return false, ErrIdentityConflict
		}
		if _, ok := c.Roots[identity.ReaderID]; ok {
			return false, ErrIdentityConflict
		}
		delete(c.Readers, identity.ReaderID)
		if c.Phase == Live {
			c.retireIfEmpty()
		}
		// Absence cannot acknowledge a lost PUT: always require a fresh CAS.
		return true, nil
	})
	if err == nil {
		r.state = readerClosed
	}
	return err
}

// RetireRoot follows the durable root's ownership fence or tombstone write.
// An absent root gets a retirement receipt to fence a delayed AttachRoot CAS.
func (s *Store) RetireRoot(ctx context.Context, setID, rootID string) error {
	if err := validateInputs(setID, rootID); err != nil {
		return err
	}
	return s.update(ctx, setID, "", func(c *catalog) (bool, error) {
		if rootID == c.Publisher.ID {
			return false, ErrIdentityConflict
		}
		if _, ok := c.Readers[rootID]; ok {
			return false, ErrIdentityConflict
		}
		r, ok := c.Roots[rootID]
		if !ok && c.requireOpen() != nil {
			return false, nil
		}
		if r.Retired {
			return false, nil
		}
		c.Roots[rootID] = root{Retired: true}
		if c.Phase == Publishing {
			c.Phase = Live
		}
		c.retireIfEmpty()
		return true, nil
	})
}

// ReleasePublisher requires the caller to establish upload completion or an
// applicable storage write fence. Root retirement alone is never sufficient.
func (s *Store) ReleasePublisher(ctx context.Context, setID, publisherID string) error {
	if err := validateInputs(setID, publisherID); err != nil {
		return err
	}
	return s.update(ctx, setID, "", func(c *catalog) (bool, error) {
		if c.Publisher.ID != publisherID {
			return false, ErrIdentityConflict
		}
		if c.Publisher.Released {
			return false, nil
		}
		c.Publisher.Released = true
		c.retireIfEmpty()
		return true, nil
	})
}

// MarkDeleting claims an empty retired generation. Replays also succeed after
// deletion has started or finished. This method deletes no payload objects.
func (s *Store) MarkDeleting(ctx context.Context, setID string) error {
	if err := validateInputs(setID); err != nil {
		return err
	}
	return s.update(ctx, setID, "", func(c *catalog) (bool, error) {
		switch c.Phase {
		case Deleting, Deleted:
			return false, nil
		case Retired:
			c.Phase = Deleting
			return true, nil
		default:
			return false, ErrInvalidTransition
		}
	})
}

// MarkDeleted records completed payload reclamation without removing the catalog.
func (s *Store) MarkDeleted(ctx context.Context, setID string) error {
	if err := validateInputs(setID); err != nil {
		return err
	}
	return s.update(ctx, setID, "", func(c *catalog) (bool, error) {
		switch c.Phase {
		case Deleted:
			return false, nil
		case Deleting:
			c.Phase = Deleted
			return true, nil
		default:
			return false, ErrInvalidTransition
		}
	})
}

func (s *Store) Inspect(ctx context.Context, setID string) (View, error) {
	if err := validateInputs(setID); err != nil {
		return View{}, err
	}
	c, _, err := s.read(ctx, setID)
	if err != nil {
		return View{}, err
	}
	v := View{SetID: c.SetID, Phase: c.Phase, Publisher: PublisherView(c.Publisher)}
	for id, r := range c.Roots {
		v.Roots = append(v.Roots, RootView{ID: id, Retired: r.Retired})
	}
	for id, r := range c.Readers {
		v.Readers = append(v.Readers, ReaderView{ID: id, RootID: r.RootID, OwnerID: r.OwnerID})
	}
	sort.Slice(v.Roots, func(i, j int) bool { return v.Roots[i].ID < v.Roots[j].ID })
	sort.Slice(v.Readers, func(i, j int) bool { return v.Readers[i].ID < v.Readers[j].ID })
	return v, nil
}

func (s *Store) update(ctx context.Context, setID, createPublisher string, mutate func(*catalog) (bool, error)) error {
	var writeErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, generation, err := s.read(ctx, setID)
		creating := errors.Is(err, ErrNotFound) && createPublisher != ""
		if creating {
			c = &catalog{Version: 1, SetID: setID, Phase: Publishing,
				Publisher: publisher{ID: createPublisher}, Roots: map[string]root{}, Readers: map[string]reader{}}
		} else if err != nil {
			return err
		}
		changed, err := mutate(c)
		if err != nil {
			return err
		}
		if !changed && !creating {
			return nil
		}
		// An ambiguous PUT gets a fresh read to recognize its result. If the
		// operation is still incomplete, return that transport error for retry.
		if writeErr != nil && !errors.Is(writeErr, gcsblob.ErrPreconditionFailed) {
			return writeErr
		}
		if attempt == maxCASAttempts {
			return fmt.Errorf("%w: %v", ErrRetryLimit, writeErr)
		}
		if c.activeHolders() > MaxActiveHolders {
			return ErrCapacity
		}
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if len(data) > MaxCatalogBytes {
			return ErrCapacity
		}
		_, writeErr = s.blob.PutBytesIfGenerationMatch(ctx, catalogKey(setID), data, generation)
		if writeErr == nil {
			return nil
		}
	}
}

func (s *Store) read(ctx context.Context, setID string) (*catalog, int64, error) {
	data, generation, err := s.blob.GetBytesGen(ctx, catalogKey(setID))
	if errors.Is(err, gcsblob.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if generation <= 0 {
		return nil, 0, fmt.Errorf("%w: nonpositive object generation", ErrInvalidCatalog)
	}
	c, err := parseCatalog(data, setID)
	return c, generation, err
}

func (c *catalog) activeHolders() int {
	n := len(c.Readers)
	if !c.Publisher.Released {
		n++
	}
	for _, r := range c.Roots {
		if !r.Retired {
			n++
		}
	}
	return n
}

func (c *catalog) retireIfEmpty() {
	if c.activeHolders() == 0 {
		c.Phase = Retired
	}
}

func (c *catalog) requireOpen() error {
	if c.Phase == Retired || c.Phase == Deleting || c.Phase == Deleted {
		return ErrRetired
	}
	return nil
}

func validateInputs(setID string, ids ...string) error {
	if err := validateUUID(setID); err != nil {
		return err
	}
	for _, id := range ids {
		if err := validateIdentity(id); err != nil {
			return err
		}
	}
	return nil
}

func validateUUID(id string) error {
	u, err := uuid.Parse(id)
	if err != nil || u == uuid.Nil || u.String() != id {
		return fmt.Errorf("%w: expected a canonical nonzero UUID", ErrInvalidID)
	}
	return nil
}

func validateIdentity(id string) error {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return ErrInvalidID
	}
	return nil
}

func parseCatalog(data []byte, setID string) (*catalog, error) {
	invalid := func(err error) (*catalog, error) {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCatalog, err)
	}
	if len(data) > MaxCatalogBytes {
		return invalid(ErrCapacity)
	}
	if !utf8.Valid(data) {
		return invalid(errors.New("invalid UTF-8"))
	}
	if err := rejectDuplicateFields(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return invalid(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var c catalog
	if err := decoder.Decode(&c); err != nil {
		return invalid(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return invalid(errors.New("trailing JSON data"))
	}
	if c.Version != 1 || c.SetID != setID || c.Roots == nil || c.Readers == nil {
		return invalid(errors.New("unsupported version, wrong set, or missing holder maps"))
	}
	if err := validateIdentity(c.Publisher.ID); err != nil {
		return invalid(err)
	}
	for id := range c.Roots {
		if err := validateIdentity(id); err != nil {
			return invalid(err)
		}
		if id == c.Publisher.ID {
			return invalid(ErrIdentityConflict)
		}
	}
	for id, r := range c.Readers {
		if err := validateUUID(id); err != nil {
			return invalid(err)
		}
		if err := validateUUID(r.OwnerID); err != nil {
			return invalid(err)
		}
		if _, ok := c.Roots[r.RootID]; !ok {
			return invalid(errors.New("reader refers to an unknown root"))
		}
		if _, ok := c.Roots[id]; ok || id == c.Publisher.ID {
			return invalid(ErrIdentityConflict)
		}
	}
	active := c.activeHolders()
	if active > MaxActiveHolders {
		return invalid(ErrCapacity)
	}
	switch c.Phase {
	case Publishing:
		if c.Publisher.Released || len(c.Roots) != 0 || len(c.Readers) != 0 {
			return invalid(errors.New("publishing catalog has invalid holders"))
		}
	case Live:
		if active == 0 || len(c.Roots) == 0 {
			return invalid(errors.New("live catalog has no active holders or root history"))
		}
	case Retired, Deleting, Deleted:
		if active != 0 {
			return invalid(errors.New("retired catalog has active holders"))
		}
	default:
		return invalid(errors.New("unknown phase"))
	}
	return &c, nil
}

// encoding/json accepts duplicate keys, which could otherwise hide references.
func rejectDuplicateFields(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("catalog JSON nesting exceeds schema")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key := token.(string)
			if seen[key] {
				return errors.New("duplicate catalog JSON field")
			}
			seen[key] = true
			if err := rejectDuplicateFields(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := rejectDuplicateFields(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
