package server

import (
	"container/list"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const defaultChunkCacheBytes int64 = 4 << 30

type cachedChunk struct {
	hash      string
	size      int64
	allocated int64
	present   bool
	pins      int
}

type chunkCacheStats struct {
	Limit, Resident, Reserved, Allocated, Residue int64
	Reclaimed, Errors, Bypassed, Ready            int64
}

// chunkCache owns cache files and temporary-write reservations under one lock.
// Fetches and returned byte slices belong to callers, independently of eviction.
type chunkCache struct {
	mu      sync.Mutex
	dir     string
	limit   int64
	owner   *os.File
	ready   bool
	closed  bool
	entries map[string]*list.Element
	lru     list.List
	used    int64
	counts  chunkCacheStats
}

func newChunkCache(dir string, limit int64) *chunkCache {
	c := &chunkCache{dir: dir, limit: max(0, limit), entries: make(map[string]*list.Element)}
	c.counts.Limit = c.limit
	if err := c.initialize(); err != nil {
		c.counts.Errors++
		fmt.Fprintf(os.Stderr, "chunk cache disabled: %v\n", err)
		return c
	}
	c.ready = true
	c.makeRoom(0)
	return c
}

func (c *chunkCache) initialize() error {
	if c.dir == "" {
		return fmt.Errorf("snapshot directory is not configured")
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(c.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return fmt.Errorf("directory already owned: %w", err)
	}
	c.owner = f
	files, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	var cached []os.FileInfo
	for _, file := range files {
		if file.Name() == ".lock" {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && cacheHash(info.Name()) {
			cached = append(cached, info)
			continue
		}
		if info.Mode().IsRegular() && cacheTemp(info.Name()) {
			if err := os.Remove(filepath.Join(c.dir, info.Name())); err == nil {
				c.counts.Reclaimed += info.Size()
				continue
			}
			c.counts.Errors++
		}
		if info.IsDir() {
			return fmt.Errorf("unexpected directory in chunk cache: %s", info.Name())
		}
		c.counts.Residue += info.Size()
		c.counts.Allocated += fileAllocatedBytes(info)
		c.used += info.Size()
	}
	sort.Slice(cached, func(i, j int) bool {
		if cached[i].ModTime().Equal(cached[j].ModTime()) {
			return cached[i].Name() < cached[j].Name()
		}
		return cached[i].ModTime().Before(cached[j].ModTime())
	})
	for _, info := range cached {
		e := &cachedChunk{hash: info.Name(), size: info.Size(), allocated: fileAllocatedBytes(info), present: true}
		c.entries[e.hash] = c.lru.PushFront(e)
		c.used += e.size
	}
	return nil
}

func cacheHash(name string) bool {
	if len(name) != 64 || name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func cacheTemp(name string) bool {
	return len(name) > 70 && name[0] == '.' && cacheHash(name[1:65]) && strings.HasPrefix(name[65:], ".tmp-")
}

func fileAllocatedBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return 0
}

func (c *chunkCache) get(hash string, size uint64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return nil
	}
	el := c.entries[hash]
	if el == nil || !el.Value.(*cachedChunk).present {
		return nil
	}
	// Limit reads even when an on-disk file has been corrupted or replaced.
	f, err := os.Open(filepath.Join(c.dir, hash))
	if err != nil {
		c.discard(el)
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(size)+1))
	_ = f.Close()
	if err != nil || !chunkMatches(raw, size, hash) {
		c.discard(el)
		return nil
	}
	c.lru.MoveToFront(el)
	return raw
}

func (c *chunkCache) put(hash string, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := int64(len(raw))
	if !c.ready || c.closed || size > c.limit || !cacheHash(hash) {
		c.counts.Bypassed++
		return
	}
	el := c.entries[hash]
	if el != nil && el.Value.(*cachedChunk).present {
		c.lru.MoveToFront(el)
		return
	}
	if el == nil {
		if !c.makeRoom(size) {
			c.counts.Bypassed++
			return
		}
		el = c.lru.PushFront(&cachedChunk{hash: hash, size: size})
		c.entries[hash] = el
		c.used += size
	}
	e := el.Value.(*cachedChunk)
	if e.size != size {
		c.counts.Bypassed++
		return
	}
	// The missing entry reserves the entire temp file before the first write.
	tmp, err := os.CreateTemp(c.dir, "."+hash+".tmp-*")
	if err == nil {
		_, err = tmp.Write(raw)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), filepath.Join(c.dir, hash))
		}
		if err != nil {
			if removeErr := os.Remove(tmp.Name()); removeErr != nil && !os.IsNotExist(removeErr) {
				// Unknown cleanup outcome disables further admission until restart.
				c.ready = false
				c.counts.Residue += size
				c.used += size
				c.counts.Allocated += allocatedBytes(tmp.Name())
				c.counts.Errors++
			}
		}
	}
	if err != nil {
		c.counts.Errors++
		if e.pins == 0 {
			c.removeEntry(el)
		}
		return
	}
	e.present = true
	e.allocated = allocatedBytes(filepath.Join(c.dir, hash))
	c.lru.MoveToFront(el)
}

// reserve holds a complete hydration image through acknowledgment. Shared hashes
// consume capacity once; a rejected reservation cannot leave partial pins behind.
func (c *chunkCache) reserve(m *chunkManifest) (func(), error) {
	want := make(map[string]int64)
	for i, e := range m.Chunks {
		if e.Hash != chunkZeroHash {
			want[e.Hash] = int64(m.chunkLen(uint64(i)))
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return nil, fmt.Errorf("chunk cache unavailable")
	}
	var pinned []*list.Element
	release := func() {
		for _, el := range pinned {
			e := el.Value.(*cachedChunk)
			e.pins--
			if e.pins == 0 && !e.present {
				c.removeEntry(el)
			}
		}
	}
	var total, missing int64
	for hash, size := range want {
		if size > c.limit-total {
			return nil, fmt.Errorf("chunk cache budget cannot hold hydration image")
		}
		total += size
		if c.entries[hash] == nil {
			missing += size
		}
	}
	for hash, size := range want {
		el := c.entries[hash]
		if el != nil && el.Value.(*cachedChunk).size != size {
			release()
			return nil, fmt.Errorf("chunk cache size mismatch for hydration")
		}
		if el == nil {
			continue
		}
		el.Value.(*cachedChunk).pins++
		pinned = append(pinned, el)
	}
	if !c.makeRoom(missing) {
		release()
		return nil, fmt.Errorf("chunk cache budget cannot hold hydration image")
	}
	for hash, size := range want {
		if c.entries[hash] == nil {
			el := c.lru.PushFront(&cachedChunk{hash: hash, size: size, pins: 1})
			c.entries[hash] = el
			c.used += size
			pinned = append(pinned, el)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			release()
		})
	}, nil
}

func (c *chunkCache) makeRoom(add int64) bool {
	for el := c.lru.Back(); el != nil && c.used > c.limit-add; {
		prev := el.Prev()
		if el.Value.(*cachedChunk).pins == 0 {
			c.discard(el)
		}
		el = prev
	}
	return c.used <= c.limit-add
}

func (c *chunkCache) discard(el *list.Element) {
	e := el.Value.(*cachedChunk)
	if e.present {
		err := os.Remove(filepath.Join(c.dir, e.hash))
		if err != nil && !os.IsNotExist(err) {
			c.counts.Errors++
			return
		}
		if err == nil {
			c.counts.Reclaimed += e.size
		}
		e.present = false
		e.allocated = 0
	}
	if e.pins == 0 {
		c.removeEntry(el)
	}
}

func (c *chunkCache) removeEntry(el *list.Element) {
	e := el.Value.(*cachedChunk)
	c.used -= e.size
	delete(c.entries, e.hash)
	c.lru.Remove(el)
}

func (c *chunkCache) stats() chunkCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	if c.ready && !c.closed {
		out.Ready = 1
	}
	for _, el := range c.entries {
		e := el.Value.(*cachedChunk)
		if e.present {
			out.Resident += e.size
			out.Allocated += e.allocated
		} else {
			out.Reserved += e.size
		}
	}
	return out
}

func (c *chunkCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.owner != nil {
		_ = c.owner.Close()
	}
}

func (s *Server) memoryChunkCache() *chunkCache {
	s.chunkCacheOnce.Do(func() {
		limit := s.cfg.UFFDChunkCacheBytes
		if limit == 0 {
			limit = defaultChunkCacheBytes
		}
		dir := ""
		if s.cfg.Provisioner != nil && s.cfg.Provisioner.SnapshotDir != "" {
			dir = s.chunkCacheDir()
		}
		s.chunkCache = newChunkCache(dir, limit)
	})
	return s.chunkCache
}
