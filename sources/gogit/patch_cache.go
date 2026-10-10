package gogit

import (
	"container/list"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
)

// Merge commits and branch histories often compare the same pair of blobs.
// Cache only the immutable text additions, never commit/path attributes or
// findings. Each use still passes through the current commit's filtering and
// scanner callback. Archives retain their ordinary streaming path.
type patchCache struct {
	mu      sync.Mutex
	entries map[patchKey]*list.Element
	lru     list.List
	bytes   int
	hits    uint64
	misses  uint64
}

type patchKey struct{ from, to plumbing.Hash }
type textAddition struct {
	raw  string
	line int
}
type cachedPatch struct {
	key       patchKey
	additions []textAddition
	bytes     int
}

const (
	patchCacheBudget = 32 << 20
	maxCachedPatch   = 256 << 10
	patchOverhead    = 384 // Key copies, map slots, list node and cachedPatch.
)

func (c *patchCache) get(key patchKey) ([]textAddition, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		c.hits++
		c.lru.MoveToFront(e)
		return e.Value.(*cachedPatch).additions, true
	}
	c.misses++
	return nil, false
}

// put takes ownership of additions. Its slice and strings must remain immutable.
func (c *patchCache) put(key patchKey, additions []textAddition) {
	if c == nil {
		return
	}
	size := patchOverhead + cap(additions)*24
	for _, a := range additions {
		size += (len(a.raw) + 15) &^ 15
	}
	if size > maxCachedPatch {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[patchKey]*list.Element)
	}
	if c.entries[key] != nil {
		return
	}
	c.entries[key] = c.lru.PushFront(&cachedPatch{key, additions, size})
	c.bytes += size
	for c.bytes > patchCacheBudget {
		e := c.lru.Back()
		value := e.Value.(*cachedPatch)
		delete(c.entries, value.key)
		c.bytes -= value.bytes
		c.lru.Remove(e)
	}
}
