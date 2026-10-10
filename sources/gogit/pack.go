package gogit

import (
	"bufio"
	"compress/zlib"
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

type objectPack struct {
	index    *idxfile.LazyIndex
	file     idxfile.ReadAtCloser
	end      int64
	hashSize int
}

func (s *objectStorage) openPacks(fs readOnlyFS) error {
	ids, err := s.Storage.ObjectPacks()
	if err != nil {
		return err
	}
	pool := fdpool.New(64)
	for _, id := range ids[:min(len(ids), 32)] {
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
			return errors.New("invalid Git pack header")
		}
		count, err := index.Count()
		if err != nil {
			return err
		}
		if int64(binary.BigEndian.Uint32(header[8:])) != count {
			return errors.New("Git pack/index object count mismatch")
		}
		var footer [32]byte
		if _, err := f.ReadAt(footer[:id.Size()], p.end); err != nil {
			return err
		}
		if id.Compare(footer[:id.Size()]) != 0 {
			return errors.New("Git pack/index checksum mismatch")
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

func (s *objectStorage) readPacked(p *objectPack, offset int64, id plumbing.Hash, depth int) (*storedObject, error) {
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
	if o, ok := s.disk.get(id); ok {
		s.objects.Put(o)
		return o, nil
	}
	kind, baseOffset, baseID, raw, err := s.inflate(p, offset)
	if err != nil {
		return nil, fmt.Errorf("pack object %s: %w", id, err)
	}
	if kind.IsDelta() {
		var base *storedObject
		if kind == plumbing.OFSDeltaObject {
			baseID, err = p.index.FindHash(baseOffset)
			if err == nil {
				base, err = s.readPacked(p, baseOffset, baseID, depth+1)
			}
		} else {
			base, err = s.read(baseID, depth+1)
		}
		if err != nil {
			return nil, err
		}
		raw, err = applyDelta(s.ctx, base.raw, raw, s.limit)
		if err != nil {
			return nil, fmt.Errorf("delta %s: %w", id, err)
		}
		kind = base.kind
	}
	o := &storedObject{id: id, kind: kind, raw: raw}
	s.disk.put(o)
	s.objects.Put(o)
	return o, nil
}

func (s *objectStorage) inflate(p *objectPack, offset int64) (kind plumbing.ObjectType, baseOffset int64, baseID plumbing.Hash, raw string, err error) {
	if offset < 12 || offset >= p.end {
		err = errors.New("Git pack offset out of bounds")
		return
	}
	inflater := packInflaters.Get().(*packInflater)
	defer func() { inflater.input.Reset(nil); packInflaters.Put(inflater) }()
	r := inflater.input
	r.Reset(io.NewSectionReader(p.file, offset, p.end-offset))
	b, e := r.ReadByte()
	if e != nil {
		err = e
		return
	}
	kind = plumbing.ObjectType((b >> 4) & 7)
	if !kind.Valid() {
		err = errors.New("invalid Git pack object type")
		return
	}
	size := uint64(b & 15)
	for shift := uint(4); b&128 != 0; shift += 7 {
		if shift >= 63 {
			err = errors.New("Git pack object size overflow")
			return
		}
		b, err = r.ReadByte()
		if err != nil {
			return
		}
		if uint64(b&127) > uint64(s.limit)>>shift {
			err = errors.New("Git pack object size exceeds reconstruction limit")
			return
		}
		size |= uint64(b&127) << shift
	}
	if size > uint64(s.limit) {
		err = fmt.Errorf("packed object size %d exceeds reconstruction limit %d", size, s.limit)
		return
	}
	switch kind {
	case plumbing.OFSDeltaObject:
		b, err = r.ReadByte()
		if err != nil {
			return
		}
		distance := uint64(b & 127)
		for b&128 != 0 {
			if distance+1 > uint64(offset)>>7 {
				err = errors.New("Git delta offset out of bounds")
				return
			}
			b, err = r.ReadByte()
			if err != nil {
				return
			}
			distance = ((distance + 1) << 7) | uint64(b&127)
		}
		if distance == 0 || distance > uint64(offset-12) {
			err = errors.New("Git delta offset out of bounds")
			return
		}
		baseOffset = offset - int64(distance)
	case plumbing.REFDeltaObject:
		baseID.ResetBySize(p.hashSize)
		_, err = baseID.ReadFrom(r)
		if err != nil {
			return
		}
	}
	if inflater.reader == nil {
		inflater.reader, err = zlib.NewReader(r)
	} else {
		err = inflater.reader.(zlib.Resetter).Reset(r, nil)
	}
	if err != nil {
		return
	}
	var out strings.Builder
	out.Grow(int(size))
	_, err = io.CopyBuffer(&out, io.LimitReader(contextReader{s.ctx, inflater.reader}, int64(size)+1), inflater.buffer[:])
	closeErr := inflater.reader.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return
	}
	if uint64(out.Len()) != size {
		err = errors.New("Git packed object size mismatch")
		return
	}
	raw = out.String()
	return
}
