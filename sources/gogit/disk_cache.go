package gogit

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
)

// diskObjectCache is a bounded, scan-local ring of materialized large objects.
// Deep delta chains can evict their own bases from the RAM cache before the
// next file uses them. Keeping those bases on disk avoids reconstructing the
// same multi-megabyte content dozens of times. Readers own their returned bytes;
// reusing a segment can never change a fragment or an object still in use.
type diskObjectCache struct {
	mu          sync.RWMutex
	file        *os.File
	failed      bool
	segmentSize int64
	segment     int
	used        int64
	entries     map[plumbing.Hash]diskObject
	keys        [8][]plumbing.Hash
}

type diskObject struct {
	offset int64
	size   int64
	kind   plumbing.ObjectType
}

func (c *diskObjectCache) get(id plumbing.Hash) (*storedObject, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[id]
	if !ok {
		return nil, false
	}
	var out strings.Builder
	out.Grow(int(e.size))
	buf := diskReadBuffers.Get().(*[256 << 10]byte)
	defer diskReadBuffers.Put(buf)
	n, err := io.CopyBuffer(&out, io.NewSectionReader(c.file, e.offset, e.size), buf[:])
	if err != nil || n != e.size {
		return nil, false
	}
	return &storedObject{id: id, kind: e.kind, raw: out.String()}, true
}

func (c *diskObjectCache) put(o *storedObject) {
	if c == nil || o.Size() < 1<<20 || o.Size() > c.segmentSize {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return
	}
	if _, ok := c.entries[o.id]; ok {
		return
	}
	if c.file == nil {
		f, err := os.CreateTemp("", "betterleaks-git-objects-*")
		if err != nil {
			c.failed = true
			return
		}
		c.file = f
		// On Unix, unlink immediately so even an interrupted process releases
		// the cache. Platforms that forbid unlinking an open file use close's
		// removal instead. All cache access uses the existing descriptor.
		_ = os.Remove(f.Name())
		c.entries = make(map[plumbing.Hash]diskObject)
	}
	if c.used+o.Size() > c.segmentSize {
		c.segment = (c.segment + 1) % len(c.keys)
		c.used = 0
		for _, id := range c.keys[c.segment] {
			delete(c.entries, id)
		}
		c.keys[c.segment] = c.keys[c.segment][:0]
	}
	offset := int64(c.segment)*c.segmentSize + c.used
	if _, err := c.file.Seek(offset, io.SeekStart); err != nil {
		c.failed = true
		return
	}
	n, err := c.file.WriteString(o.raw)
	if err != nil || n != len(o.raw) {
		c.failed = true
		return
	}

	c.entries[o.id] = diskObject{offset: offset, size: o.Size(), kind: o.kind}
	c.keys[c.segment] = append(c.keys[c.segment], o.id)
	c.used += o.Size()
}

func (c *diskObjectCache) close() {
	if c == nil || c.file == nil {
		return
	}
	c.file.Close()
	os.Remove(c.file.Name())
}

var diskReadBuffers = sync.Pool{New: func() any { return new([256 << 10]byte) }}
