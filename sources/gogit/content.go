package gogit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/mholt/archives"
	"github.com/sergi/go-diff/diffmatchpatch"
)

func (w *worker) change(ctx context.Context, c *object.Change, attrs map[string]string, emit func(string, int) error, yield sources.FragmentsFunc) error {
	kindChanged := c.From.Name != "" && uint32(c.From.TreeEntry.Mode)&0170000 != uint32(c.To.TreeEntry.Mode)&0170000
	if c.From.Name != "" && c.From.TreeEntry.Hash == c.To.TreeEntry.Hash && !kindChanged {
		return nil
	}
	dst, binary, err := w.content(ctx, c.To)
	if err != nil {
		return err
	}
	if binary {
		return w.archive(ctx, c.To, attrs, yield)
	}
	if c.From.Name == "" || kindChanged {
		return emit(dst, 1)
	}
	src, binary, err := w.content(ctx, c.From)
	if err != nil {
		return err
	}
	if binary {
		return nil
	}
	return lineAdditions(ctx, src, dst, emit)
}

// Lines absent from the other file cannot be matches. Remove them before the
// quadratic edit search, then map matching destination lines back to their
// original positions. This matters for generated files with tens of thousands
// of replaced lines; diffing those unmatched lines wastes almost all CPU time.
// As with Git's own preprocessing, ambiguous edit boundaries can differ from
// an unpruned diff. We preserve exact destination contents and line numbers.
func lineAdditions(ctx context.Context, src, dst string, emit func(string, int) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Tokens must fit the edit engine's Unicode alphabet. This also bounds the
	// bookkeeping allocated for a diff independently of its input byte limit.
	if strings.Count(src, "\n")+strings.Count(dst, "\n")+2 > 1_000_000 {
		return errors.New("diff inputs exceed the one-million-line token limit")
	}
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		dmp.DiffTimeout = min(dmp.DiffTimeout, max(time.Until(deadline), time.Nanosecond))
	}
	// Only parent lines can be matches. Tokenize them once; destination-only
	// lines need neither IDs nor strings in the dictionary. Use valid Unicode
	// tokens because the edit engine serializes rune sequences as strings.
	lineCount := func(text string) int {
		n := strings.Count(text, "\n")
		if len(text) > 0 && text[len(text)-1] != '\n' {
			n++
		}
		return n
	}
	oldCount, newCount := lineCount(src), lineCount(dst)
	ids := make(map[string]rune, min(oldCount, 65536))
	a := make([]rune, 0, oldCount)
	for offset := 0; offset < len(src); {
		end := strings.IndexByte(src[offset:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += offset + 1
		}
		line := src[offset:end]
		id, ok := ids[line]
		if !ok {
			id = rune(len(ids) + 1)
			if id >= 0xd800 {
				id += 0x800
			}
			ids[line] = id
		}
		a = append(a, id)
		offset = end
	}
	present := make([]bool, len(ids)+1)
	current := make([]rune, 0, newCount)
	positions := make([]int, 0, newCount)
	for offset, pos := 0, 0; offset < len(dst); pos++ {
		end := strings.IndexByte(dst[offset:], '\n')
		if end < 0 {
			end = len(dst)
		} else {
			end += offset + 1
		}
		if id, ok := ids[dst[offset:end]]; ok {
			index := id
			if index >= 0xe000 {
				index -= 0x800
			}
			present[index] = true
			current = append(current, id)
			positions = append(positions, pos)
		}
		offset = end
	}
	old := a[:0]
	for _, id := range a {
		index := id
		if index >= 0xe000 {
			index -= 0x800
		}
		if present[index] {
			old = append(old, id)
		}
	}
	if len(current) == 0 {
		if dst == "" {
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(dst, 1)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	added := make([]bool, newCount)
	for i := range added {
		added[i] = true
	}
	at := 0
	for _, chunk := range dmp.DiffMainRunes(old, current, false) {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := utf8.RuneCountInString(chunk.Text)
		switch chunk.Type {
		case diffmatchpatch.DiffEqual:
			for _, pos := range positions[at : at+n] {
				added[pos] = false
			}
			at += n
		case diffmatchpatch.DiffInsert:
			at += n
		}
	}
	// Slice original bytes directly. No equal/deleted contents are reconstructed,
	// and the final unterminated line retains its original bytes. Clone emitted
	// slices so a small queued addition cannot keep an entire large blob alive.
	offset, begin, first := 0, -1, 0
	for i, yes := range added {
		if yes && begin < 0 {
			begin = offset
			first = i + 1
		}
		if !yes && begin >= 0 {
			if err := emit(strings.Clone(dst[begin:offset]), first); err != nil {
				return err
			}
			begin = -1
		}
		end := strings.IndexByte(dst[offset:], '\n')
		if end < 0 {
			offset = len(dst)
		} else {
			offset += end + 1
		}
	}
	if begin >= 0 {
		if begin == 0 && offset == len(dst) {
			return emit(dst, first)
		}
		return emit(strings.Clone(dst[begin:offset]), first)
	}
	return ctx.Err()
}

func (w *worker) content(ctx context.Context, e object.ChangeEntry) (string, bool, error) {
	if e.TreeEntry.Mode == filemode.Submodule {
		return "Subproject commit " + e.TreeEntry.Hash.String() + "\n", false, nil
	}
	if obj, ok := w.objects.Get(e.TreeEntry.Hash); ok {
		if text, ok := obj.(*textBlob); ok {
			return text.raw, false, nil
		}
	}
	f, err := e.Tree.TreeEntryFile(&e.TreeEntry)
	if err != nil {
		return "", false, err
	}
	if f.Size < 0 || f.Size > w.maxBlob {
		return "", false, fmt.Errorf("blob %s size %d exceeds diff input limit %d", e.TreeEntry.Hash, f.Size, w.maxBlob)
	}
	r, err := f.Reader()
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	reader := contextReader{ctx: ctx, r: r}
	prefix := w.buffer[:8000]
	n, err := io.ReadFull(reader, prefix)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, err
	}
	if strings.IndexByte(string(prefix[:n]), 0) >= 0 {
		return "", true, nil
	}
	var text strings.Builder
	text.Grow(int(f.Size))
	text.Write(prefix[:n])
	_, err = io.CopyBuffer(&text, reader, w.buffer[:])
	if err != nil {
		return "", false, err
	}
	if int64(text.Len()) != f.Size {
		return "", false, fmt.Errorf("blob %s size mismatch", e.TreeEntry.Hash)
	}
	raw := text.String()
	// Replace the materialized byte slice with the string we already own. This
	// uses the same cache budget, rather than retaining a second blob cache.
	w.objects.Put(&textBlob{id: e.TreeEntry.Hash, raw: raw})
	return raw, false, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func (w *worker) archive(ctx context.Context, e object.ChangeEntry, attrs map[string]string, yield sources.FragmentsFunc) error {
	if w.source.MaxArchiveDepth <= 0 {
		return nil
	}
	format, _, err := archives.Identify(ctx, e.Name, nil)
	if errors.Is(err, archives.NoMatch) {
		return nil
	}
	if err != nil {
		return err
	}
	if format == nil {
		return nil
	}
	file, err := e.Tree.TreeEntryFile(&e.TreeEntry)
	if err != nil {
		return err
	}
	r, err := file.Reader()
	if err != nil {
		return err
	}
	defer r.Close()
	source := sources.File{Logger: w.source.Logger, Content: contextReader{ctx, r}, Path: e.Name, Attributes: attrs, Prefilter: w.source.Prefilter, MaxArchiveDepth: w.source.MaxArchiveDepth, StrictArchives: true}
	return source.Fragments(ctx, yield)
}

// textBlob is immutable and can also serve as an upstream delta base. Cached
// text bypasses repeat decoding and byte-to-string copies, never path filtering.
type textBlob struct {
	id  plumbing.Hash
	raw string
}

func (b *textBlob) Hash() plumbing.Hash       { return b.id }
func (b *textBlob) Type() plumbing.ObjectType { return plumbing.BlobObject }
func (b *textBlob) Size() int64               { return int64(len(b.raw)) }
func (*textBlob) SetType(plumbing.ObjectType) {}
func (*textBlob) SetSize(int64)               {}
func (b *textBlob) Reader() (io.ReadCloser, error) {
	return textBlobReader{strings.NewReader(b.raw)}, nil
}
func (*textBlob) Writer() (io.WriteCloser, error) {
	return nil, errors.New("cached Git blob is read-only")
}

type textBlobReader struct{ *strings.Reader }

func (textBlobReader) Close() error { return nil }
