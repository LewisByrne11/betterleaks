package gogit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// objectStorage keeps go-git's repository discovery, indexes and loose-object
// support, but materializes deltas directly into immutable strings. The normal
// reader copies the base, builds a temporary result, copies it into MemoryObject,
// then hashes that result before the source copies it into a string again.
type objectStorage struct {
	*filesystem.Storage
	objects *objectCache
	ctx     context.Context
	limit   int64
	packs   []*objectPack
	disk    *diskObjectCache
}

func (s *objectStorage) EncodedObject(kind plumbing.ObjectType, id plumbing.Hash) (plumbing.EncodedObject, error) {
	o, err := s.read(id, 0)
	if err != nil {
		return nil, err
	}
	if kind != plumbing.AnyObject && o.Type() != kind {
		return nil, plumbing.ErrObjectNotFound
	}
	return o, nil
}

func (s *objectStorage) read(id plumbing.Hash, depth int) (*storedObject, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if depth > 128 {
		return nil, errors.New("Git delta chain exceeds 128 levels")
	}
	if o, ok := s.objects.Get(id); ok {
		if o, ok := o.(*storedObject); ok {
			return o, nil
		}
	}
	for _, p := range s.packs {
		offset, err := p.index.FindOffset(id)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return s.readPacked(p, offset, id, depth)
	}
	// Retain upstream's ordinary path for loose objects, alternates and packs
	// without a reverse index. DeltaObject probes loose files even for every
	// packed delta base, which is substantially slower on these fallback paths.
	o, err := s.Storage.EncodedObject(plumbing.AnyObject, id)
	if err != nil {
		return nil, err
	}
	raw, err := readObjectString(s.ctx, o, s.limit)
	if err != nil {
		return nil, fmt.Errorf("object %s: %w", id, err)
	}
	result := &storedObject{id: id, kind: o.Type(), raw: raw}
	s.objects.Put(result)
	return result, nil
}

var objectReadBuffers = sync.Pool{New: func() any { return new([32 << 10]byte) }}

func readObjectString(ctx context.Context, obj plumbing.EncodedObject, limit int64) (string, error) {
	if obj.Size() < 0 || obj.Size() > limit {
		return "", fmt.Errorf("object size %d exceeds reconstruction limit %d", obj.Size(), limit)
	}
	r, err := obj.Reader()
	if err != nil {
		return "", err
	}
	defer r.Close()
	var b strings.Builder
	b.Grow(int(obj.Size()))
	buf := objectReadBuffers.Get().(*[32 << 10]byte)
	defer objectReadBuffers.Put(buf)
	_, err = io.CopyBuffer(&b, io.LimitReader(contextReader{ctx, r}, obj.Size()+1), buf[:])
	if err != nil {
		return "", err
	}
	if int64(b.Len()) != obj.Size() {
		return "", errors.New("Git object size mismatch")
	}
	return b.String(), nil
}

type storedObject struct {
	id   plumbing.Hash
	kind plumbing.ObjectType
	raw  string
}

func (o *storedObject) Hash() plumbing.Hash       { return o.id }
func (o *storedObject) Type() plumbing.ObjectType { return o.kind }
func (o *storedObject) Size() int64               { return int64(len(o.raw)) }
func (*storedObject) SetType(plumbing.ObjectType) {}
func (*storedObject) SetSize(int64)               {}
func (o *storedObject) Reader() (io.ReadCloser, error) {
	return objectStringReader{strings.NewReader(o.raw)}, nil
}
func (*storedObject) Writer() (io.WriteCloser, error) {
	return nil, errors.New("cached Git object is read-only")
}

// applyDelta validates the complete instruction stream before allocating its
// declared result, then copies directly from the immutable base and literals.
// It never retains a chain of full-sized intermediate scratch buffers.
func applyDelta(ctx context.Context, base, delta string, limit int64) (string, error) {
	readSize := func() (uint64, error) {
		var n uint64
		for shift := uint(0); shift < 63 && len(delta) > 0; shift += 7 {
			b := delta[0]
			delta = delta[1:]
			n |= uint64(b&127) << shift
			if b&128 == 0 {
				return n, nil
			}
		}
		return 0, errors.New("invalid Git delta size")
	}
	sourceSize, err := readSize()
	if err != nil {
		return "", err
	}
	if sourceSize != uint64(len(base)) {
		return "", errors.New("Git delta base size mismatch")
	}
	targetSize, err := readSize()
	if err != nil {
		return "", err
	}
	if targetSize > uint64(limit) {
		return "", fmt.Errorf("delta size %d exceeds reconstruction limit %d", targetSize, limit)
	}
	var out strings.Builder
	for pass := 0; pass < 2; pass++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if pass == 1 {
			out.Grow(int(targetSize))
		}
		remaining := targetSize
		for at, ops := 0, 0; at < len(delta); ops++ {
			if ops&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return "", err
				}
			}
			cmd := delta[at]
			at++
			if cmd&128 != 0 {
				var offset, size uint64
				for j := uint(0); j < 7; j++ {
					if cmd&(1<<j) == 0 {
						continue
					}
					if at == len(delta) {
						return "", errors.New("truncated Git delta copy")
					}
					v := uint64(delta[at])
					at++
					if j < 4 {
						offset |= v << (j * 8)
					} else {
						size |= v << ((j - 4) * 8)
					}
				}
				if size == 0 {
					size = 65536
				}
				if size > remaining || offset > sourceSize || size > sourceSize-offset {
					return "", errors.New("Git delta copy out of bounds")
				}
				if pass == 1 {
					out.WriteString(base[offset : offset+size])
				}
				remaining -= size
			} else {
				n := int(cmd)
				if n == 0 || n > len(delta)-at || uint64(n) > remaining {
					return "", errors.New("invalid Git delta literal")
				}
				if pass == 1 {
					out.WriteString(delta[at : at+n])
				}
				at += n
				remaining -= uint64(n)
			}
		}
		if remaining != 0 {
			return "", errors.New("Git delta result size mismatch")
		}
	}
	return out.String(), nil
}

type objectStringReader struct{ *strings.Reader }

func (objectStringReader) Close() error { return nil }
