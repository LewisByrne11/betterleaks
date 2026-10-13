package gogit

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// go-git's All iterator materializes every commit in a linked list, and stops a
// ref walk at the first shared commit even when other merge ancestry is pending.
// Keep one shared visited set and a frontier of IDs/timestamps instead. Decoded
// commits (and their retained pack readers) live only while a worker needs them.
type history struct {
	r        *git.Repository
	seen     map[plumbing.Hash]commitResources
	scope    commitResources
	selected int
	frontier commitHeap
	sequence uint64
}
type commitResources uint8

const (
	scanMessages commitResources = 1 << iota
	scanPatches
	commitSelected // Counts a commit once across resource passes.
)

type commitPosition struct {
	id        plumbing.Hash
	when      int64
	sequence  uint64
	resources commitResources
}
type commitHeap []commitPosition

func (h commitHeap) Len() int { return len(h) }
func (h commitHeap) Less(i, j int) bool {
	if h[i].when == h[j].when {
		return h[i].sequence < h[j].sequence
	}
	return h[i].when > h[j].when
}
func (h commitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *commitHeap) Push(v any)   { *h = append(*h, v.(commitPosition)) }
func (h *commitHeap) Pop() any {
	last := len(*h) - 1
	v := (*h)[last]
	*h = (*h)[:last]
	return v
}

func (s *Source) history(ctx context.Context, r *git.Repository, scope commitResources) (*history, error) {
	h := &history{r: r, seen: make(map[plumbing.Hash]commitResources), scope: scope, selected: 0, frontier: nil, sequence: 0}
	if !s.All {
		revision := s.Revision
		if revision == "" {
			revision = "HEAD"
		}
		id, err := r.ResolveRevision(plumbing.Revision(revision))
		if err != nil {
			// An unborn default HEAD has no history, but may have staged loose
			// blobs or notes. Explicit missing revisions remain errors.
			if s.Revision == "" && errors.Is(err, plumbing.ErrReferenceNotFound) {
				return h, nil
			}
			return nil, fmt.Errorf("resolve revision %q: %w", revision, err)
		}
		if err = h.tip(ctx, *id); err != nil {
			return nil, err
		}
		return h, nil
	}
	head, err := r.Head()
	if err == nil {
		if err = h.tip(ctx, head.Hash()); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}
	refs, err := r.Storer.IterReferences()
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, err := refs.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(ref.Name().String(), "refs/notes/") {
			continue
		}
		// IterReferences can include symbolic remote HEADs. Resolve them rather than
		// interpreting their zero hash as an object ID.
		if ref.Type() == plumbing.SymbolicReference {
			name := ref.Name()
			ref, err = storer.ResolveReference(r.Storer, name)
			if err != nil {
				if name == plumbing.HEAD && errors.Is(err, plumbing.ErrReferenceNotFound) {
					continue // Unborn HEAD; there may still be notes or loose blobs.
				}
				return nil, fmt.Errorf("resolve ref %s: %w", name, err)
			}
		}
		if err = h.tip(ctx, ref.Hash()); err != nil {
			return nil, fmt.Errorf("ref %s: %w", ref.Name(), err)
		}
	}
	return h, nil
}
func (h *history) tip(ctx context.Context, id plumbing.Hash) error {
	for depth := 0; depth < 32; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if h.seen[id]&h.scope == h.scope {
			return nil
		}
		obj, err := h.r.Storer.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return fmt.Errorf("read history tip %s: %w", id, err)
		}
		switch obj.Type() {
		case plumbing.CommitObject:
			c, err := object.DecodeCommit(h.r.Storer, obj)
			if err != nil {
				return err
			}
			h.enqueue(c)
			return nil
		case plumbing.TagObject:
			tag, err := object.DecodeTag(h.r.Storer, obj)
			if err != nil {
				return err
			}
			id = tag.Target
		case plumbing.BlobObject, plumbing.TreeObject:
			// Valid tags can name non-commit objects, which have no commit ancestry.
			return nil
		default:
			return fmt.Errorf("invalid history tip type %s", obj.Type())
		}
	}
	return errors.New("annotated tag nesting exceeds 32 levels")
}
func (h *history) enqueue(c *object.Commit) {
	resources := h.scope &^ h.seen[c.Hash]
	h.seen[c.Hash] |= resources
	heap.Push(&h.frontier, commitPosition{c.Hash, c.Committer.When.Unix(), h.sequence, resources})
	h.sequence++
}
func (h *history) next(ctx context.Context) (*object.Commit, commitResources, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if len(h.frontier) == 0 {
		return nil, 0, io.EOF
	}
	p := heap.Pop(&h.frontier).(commitPosition)
	c, err := h.r.CommitObject(p.id)
	if err != nil {
		return nil, 0, err
	}
	for _, id := range c.ParentHashes {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if h.seen[id]&h.scope == h.scope {
			continue
		}
		parent, err := h.r.CommitObject(id)
		if err != nil {
			return nil, 0, fmt.Errorf("parent %s of %s: %w", id, c.Hash, err)
		}
		h.enqueue(parent)
	}
	return c, p.resources, nil
}
