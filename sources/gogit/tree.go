package gogit

import (
	"container/list"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Only Entries and Hash are shared. Never call Tree.FindEntry/Tree.Tree on a
// cached tree: those methods can lazily mutate the tree's internal path cache.
// 32 MiB includes an estimate for decoded entries, names, and cache overhead.
type treeCache struct {
	mu      sync.Mutex
	entries map[plumbing.Hash]*list.Element
	lru     list.List
	bytes   int
	r       *git.Repository
}
type cachedTree struct {
	id    plumbing.Hash
	tree  *object.Tree
	bytes int
}

func (c *treeCache) get(id plumbing.Hash) (*object.Tree, error) {
	c.mu.Lock()
	if e := c.entries[id]; e != nil {
		c.lru.MoveToFront(e)
		t := e.Value.(*cachedTree).tree
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	t, err := c.r.TreeObject(id)
	if err != nil {
		return nil, err
	}
	// Count allocated slice capacity, not only its populated entries. Go's
	// append growth can otherwise undercharge large decoded trees substantially.
	size := 256 + cap(t.Entries)*int(reflect.TypeFor[object.TreeEntry]().Size())
	for _, entry := range t.Entries {
		size += (len(entry.Name) + 15) &^ 15
	}
	if size > 8<<20 {
		return t, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[plumbing.Hash]*list.Element)
	}
	if e := c.entries[id]; e != nil {
		c.lru.MoveToFront(e)
		return e.Value.(*cachedTree).tree, nil
	}
	c.entries[id] = c.lru.PushFront(&cachedTree{id, t, size})
	c.bytes += size
	for c.bytes > 32<<20 {
		e := c.lru.Back()
		value := e.Value.(*cachedTree)
		delete(c.entries, value.id)
		c.bytes -= value.bytes
		c.lru.Remove(e)
	}
	return t, nil
}

func treeChanges(ctx context.Context, c *object.Commit, cache *treeCache) (object.Changes, error) {
	changes, err := rawTreeChanges(ctx, c, cache)
	if err != nil {
		return nil, err
	}
	// TreeEntryFile enforces checkout-safe names, even though rename detection
	// only reads blobs by ID. Historical paths (including a literal backslash
	// in GitLab) need not be portable checkout paths. Give those blob handles a
	// harmless name; ChangeEntry.Name still holds the actual path used to score
	// renames and emit metadata. No tree content is materialized on disk.
	for _, change := range changes {
		if change.From.Name != "" {
			change.From.TreeEntry.Name = "blob"
		}
		if change.To.Name != "" {
			change.To.TreeEntry.Name = "blob"
		}
	}
	changes, err = object.DetectRenames(changes, &object.DiffTreeOptions{DetectRenames: true, RenameScore: 50, RenameLimit: 1000})
	for _, change := range changes {
		if change.From.Name != "" {
			change.From.TreeEntry.Name = change.From.Name[strings.LastIndexByte(change.From.Name, '/')+1:]
		}
		if change.To.Name != "" {
			change.To.TreeEntry.Name = change.To.Name[strings.LastIndexByte(change.To.Name, '/')+1:]
		}
	}
	return changes, err
}

func rawTreeChanges(ctx context.Context, c *object.Commit, cache *treeCache) (object.Changes, error) {
	var old *object.Tree
	if len(c.ParentHashes) > 0 {
		parent, err := c.Parent(0)
		if err != nil {
			return nil, err
		}
		if parent.TreeHash == c.TreeHash {
			return nil, nil
		}
		old, err = cache.get(parent.TreeHash)
		if err != nil {
			return nil, err
		}
	}
	current, err := cache.get(c.TreeHash)
	if err != nil {
		return nil, err
	}
	var changes object.Changes
	// A small tree DAG can expand into exponentially many paths. Bound each
	// comparison (workers release it before taking another commit), including
	// empty directories and path bytes, not just the number of unique objects.
	const maxEntries, maxChanges, maxPathBytes = 1_000_000, 100_000, 16 << 20
	entries, pathBytes := 0, 0
	charge := func(nameBytes int) error {
		entries++
		pathBytes += nameBytes
		if entries > maxEntries || len(changes) >= maxChanges || pathBytes > maxPathBytes {
			return fmt.Errorf("git tree comparison exceeds expansion budget (%d entries, %d changes, %d path bytes)", maxEntries, maxChanges, maxPathBytes)
		}
		return ctx.Err()
	}
	var walk func(*object.Tree, *object.Tree, string) error
	var expand func(*object.Tree, object.TreeEntry, string, bool) error
	expand = func(tree *object.Tree, entry object.TreeEntry, prefix string, added bool) error {
		if err := charge(len(prefix) + len(entry.Name)); err != nil {
			return err
		}
		name := prefix + entry.Name
		if entry.Mode == filemode.Dir {
			sub, err := cache.get(entry.Hash)
			if err != nil {
				return err
			}
			if added {
				return walk(nil, sub, name+"/")
			}
			return walk(sub, nil, name+"/")
		}
		change := &object.Change{}
		value := object.ChangeEntry{Name: name, Tree: tree, TreeEntry: entry}
		if added {
			change.To = value
		} else {
			change.From = value
		}
		changes = append(changes, change)
		return nil
	}
	walk = func(from, to *object.Tree, prefix string) error {
		if strings.Count(prefix, "/") > 256 {
			return fmt.Errorf("Git tree nesting exceeds 256 levels")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if from != nil && to != nil && from.Hash == to.Hash {
			return nil
		}
		var a, b []object.TreeEntry
		if from != nil {
			a = from.Entries
		}
		if to != nil {
			b = to.Entries
		}
		i, j := 0, 0
		for i < len(a) || j < len(b) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if i == len(a) {
				if err := expand(to, b[j], prefix, true); err != nil {
					return err
				}
				j++
				continue
			}
			if j == len(b) {
				if err := expand(from, a[i], prefix, false); err != nil {
					return err
				}
				i++
				continue
			}
			left, right := a[i], b[j]
			if left.Name == right.Name {
				if err := charge(2 * (len(prefix) + len(left.Name))); err != nil {
					return err
				}
				i++
				j++
				if left.Mode == right.Mode && left.Hash == right.Hash {
					continue
				}
				if left.Mode == filemode.Dir && right.Mode == filemode.Dir {
					x, err := cache.get(left.Hash)
					if err != nil {
						return err
					}
					y, err := cache.get(right.Hash)
					if err != nil {
						return err
					}
					if err := walk(x, y, prefix+left.Name+"/"); err != nil {
						return err
					}
				} else if left.Mode == filemode.Dir || right.Mode == filemode.Dir {
					if err := expand(from, left, prefix, false); err != nil {
						return err
					}
					if err := expand(to, right, prefix, true); err != nil {
						return err
					}
				} else {
					changes = append(changes, &object.Change{From: object.ChangeEntry{Name: prefix + left.Name, Tree: from, TreeEntry: left}, To: object.ChangeEntry{Name: prefix + right.Name, Tree: to, TreeEntry: right}})
				}
				continue
			}
			// Git sorts directories as if their names ended in '/', rather than by
			// plain basename. Both input entry slices already have that ordering.
			x, y := left.Name, right.Name
			if left.Mode == filemode.Dir {
				x += "/"
			}
			if right.Mode == filemode.Dir {
				y += "/"
			}
			if strings.Compare(x, y) < 0 {
				if err := expand(from, left, prefix, false); err != nil {
					return err
				}
				i++
			} else {
				if err := expand(to, right, prefix, true); err != nil {
					return err
				}
				j++
			}
		}
		return nil
	}
	if err := walk(old, current, ""); err != nil {
		return nil, err
	}
	return changes, nil
}
