package gogit

import (
	"bufio"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	"github.com/go-git/go-git/v6/x/fdpool"
)

// Packs beyond this many use go-git's reader. Each mapped pack holds two file
// handles, and repositories with this many packs are due for a repack anyway.
const maxMappedPacks = 32

type objectPack struct {
	index    *idxfile.LazyIndex
	file     idxfile.ReadAtCloser
	end      int64
	hashSize int
}

func (s *objectStorage) openPacks(fs readOnlyFS) error {
	ids, err := s.ObjectPacks()
	if err != nil {
		return err
	}
	pool := fdpool.New(64)
	for _, id := range ids[:min(len(ids), maxMappedPacks)] {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		path := "objects/pack/pack-" + id.String()
		index, err := idxfile.NewLazyIndexWithPool(
			func() (idxfile.ReadAtCloser, error) { return fs.Open(path + ".idx") },
			func() (idxfile.ReadAtCloser, error) { return fs.Open(path + ".rev") }, id, pool)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		f, err := fs.Open(path + ".pack")
		if err != nil {
			index.Close()
			return err
		}
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			index.Close()
			return err
		}
		p := &objectPack{index: index, file: f, end: end - int64(id.Size()), hashSize: id.Size()}
		s.packs = append(s.packs, p)
		var header [12]byte
		if _, err := f.ReadAt(header[:], 0); err != nil {
			return err
		}
		version := binary.BigEndian.Uint32(header[4:8])
		if string(header[:4]) != "PACK" || (version != 2 && version != 3) {
			return errors.New("invalid git pack header")
		}
		count, err := index.Count()
		if err != nil {
			return err
		}
		if int64(binary.BigEndian.Uint32(header[8:])) != count {
			return errors.New("git pack/index object count mismatch")
		}
		var footer [32]byte
		if _, err := f.ReadAt(footer[:id.Size()], p.end); err != nil {
			return err
		}
		if id.Compare(footer[:id.Size()]) != 0 {
			return errors.New("git pack/index checksum mismatch")
		}
	}
	return nil
}

func (s *objectStorage) closePacks() {
	s.disk.close()
	for _, p := range s.packs {
		p.index.Close()
		p.file.Close()
	}
	s.packs = nil
}

type packInflater struct {
	input  *bufio.Reader
	reader io.ReadCloser
	buffer [32 << 10]byte
}

var packInflaters = sync.Pool{New: func() any { return &packInflater{input: bufio.NewReaderSize(nil, 8192)} }}

// readPacked reconstructs a packed object. It walks the delta chain down to a
// cached or undeltified base reading only record headers, then inflates and
// applies one delta at a time back up. Chain depth costs one position per
// link, never stack frames or retained delta instructions.
func (s *objectStorage) readPacked(p *objectPack, offset int64, id plumbing.Hash) (*storedObject, error) {
	type link struct {
		id     plumbing.Hash
		pack   *objectPack
		offset int64
	}
	var chain []link
	var base *storedObject
	for base == nil {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if base = s.cached(id); base != nil {
			break
		}
		if o, ok := s.disk.get(id); ok {
			s.objects.Put(o)
			base = o
			break
		}
		e, err := s.inflate(p, offset, false)
		if err != nil {
			return nil, fmt.Errorf("pack object %s: %w", id, err)
		}
		if !e.kind.IsDelta() {
			base = &storedObject{id: id, kind: e.kind, raw: e.raw}
			s.keep(base)
			break
		}
		if len(chain) == maxDeltaChain {
			return nil, fmt.Errorf("git delta chain for %s exceeds %d links", chain[0].id, maxDeltaChain)
		}
		chain = append(chain, link{id, p, offset})
		if e.kind == plumbing.OFSDeltaObject {
			offset = e.baseOffset
			if id, err = p.index.FindHash(offset); err != nil {
				return nil, err
			}
			continue
		}
		id = e.baseID
		next, nextOffset, err := s.locate(id)
		if err != nil {
			return nil, err
		}
		if next == nil {
			if base, err = s.readFallback(id); err != nil {
				return nil, err
			}
			break
		}
		p, offset = next, nextOffset
	}
	for i := len(chain) - 1; i >= 0; i-- {
		l := chain[i]
		e, err := s.inflate(l.pack, l.offset, true)
		if err != nil {
			return nil, fmt.Errorf("pack object %s: %w", l.id, err)
		}
		raw, err := applyDelta(s.ctx, base.raw, e.raw, s.limit)
		if err != nil {
			return nil, fmt.Errorf("delta %s: %w", l.id, err)
		}
		base = &storedObject{id: l.id, kind: base.kind, raw: raw}
		s.keep(base)
	}
	return base, nil
}

// packEntry is one inflated pack record. Deltas name their base by offset
// (OFS) or by object ID (REF).
type packEntry struct {
	kind       plumbing.ObjectType
	baseOffset int64
	baseID     plumbing.Hash
	raw        string
}

// inflate reads the record at offset. A delta's instructions are inflated only
// when deltas is set; otherwise its header is enough to find the base.
func (s *objectStorage) inflate(p *objectPack, offset int64, deltas bool) (packEntry, error) {
	var e packEntry
	if offset < 12 || offset >= p.end {
		return e, errors.New("git pack offset out of bounds")
	}
	inflater := packInflaters.Get().(*packInflater)
	defer func() { inflater.input.Reset(nil); packInflaters.Put(inflater) }()
	r := inflater.input
	r.Reset(io.NewSectionReader(p.file, offset, p.end-offset))
	b, err := r.ReadByte()
	if err != nil {
		return e, err
	}
	e.kind = plumbing.ObjectType((b >> 4) & 7)
	if !e.kind.Valid() {
		return e, errors.New("invalid git pack object type")
	}
	size := uint64(b & 15)
	for shift := uint(4); b&128 != 0; shift += 7 {
		if shift >= 63 {
			return e, errors.New("git pack object size overflow")
		}
		if b, err = r.ReadByte(); err != nil {
			return e, err
		}
		if uint64(b&127) > uint64(s.limit)>>shift {
			return e, limitf("packed object exceeds reconstruction limit %d", s.limit)
		}
		size |= uint64(b&127) << shift
	}
	if size > uint64(s.limit) {
		return e, limitf("packed object size %d exceeds reconstruction limit %d", size, s.limit)
	}
	switch e.kind {
	case plumbing.OFSDeltaObject:
		if b, err = r.ReadByte(); err != nil {
			return e, err
		}
		distance := uint64(b & 127)
		for b&128 != 0 {
			if distance+1 > uint64(offset)>>7 {
				return e, errors.New("git delta offset out of bounds")
			}
			if b, err = r.ReadByte(); err != nil {
				return e, err
			}
			distance = ((distance + 1) << 7) | uint64(b&127)
		}
		if distance == 0 || distance > uint64(offset-12) {
			return e, errors.New("git delta offset out of bounds")
		}
		e.baseOffset = offset - int64(distance)
	case plumbing.REFDeltaObject:
		e.baseID.ResetBySize(p.hashSize)
		if _, err = e.baseID.ReadFrom(r); err != nil {
			return e, err
		}
	}
	if e.kind.IsDelta() && !deltas {
		return e, nil
	}
	if inflater.reader == nil {
		inflater.reader, err = zlib.NewReader(r)
	} else {
		err = inflater.reader.(zlib.Resetter).Reset(r, nil)
	}
	if err != nil {
		return e, err
	}
	var out strings.Builder
	out.Grow(int(size))
	_, err = io.CopyBuffer(&out, io.LimitReader(contextReader{s.ctx, inflater.reader}, int64(size)+1), inflater.buffer[:])
	if closeErr := inflater.reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return e, err
	}
	if uint64(out.Len()) != size {
		return e, errors.New("git packed object size mismatch")
	}
	e.raw = out.String()
	return e, nil
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
		return "", errors.New("git delta base size mismatch")
	}
	targetSize, err := readSize()
	if err != nil {
		return "", err
	}
	if targetSize > uint64(limit) {
		return "", limitf("delta size %d exceeds reconstruction limit %d", targetSize, limit)
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
					return "", errors.New("git delta copy out of bounds")
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
			return "", errors.New("git delta result size mismatch")
		}
	}
	return out.String(), nil
}
