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
	// Upstream's tokenizer encodes IDs as Unicode runes and panics beyond its
	// alphabet. Bound the possible distinct lines before calling it.
	if strings.Count(src, "\n")+strings.Count(dst, "\n")+2 > 1_000_000 {
		return errors.New("diff inputs exceed the one-million-line token limit")
	}
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		dmp.DiffTimeout = min(dmp.DiffTimeout, max(time.Until(deadline), time.Nanosecond))
	}
	a, b, _ := dmp.DiffLinesToRunes(src, dst)
	present := make(map[rune]uint8, min(len(a)+len(b), 65536))
	for _, token := range a {
		present[token] |= 1
	}
	for _, token := range b {
		present[token] |= 2
	}
	old := make([]rune, 0, len(a))
	for _, token := range a {
		if present[token] == 3 {
			old = append(old, token)
		}
	}
	current := make([]rune, 0, len(b))
	positions := make([]int, 0, len(b))
	for i, token := range b {
		if present[token] == 3 {
			current = append(current, token)
			positions = append(positions, i)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	added := make([]bool, len(b))
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
	// and the final unterminated line retains its original bytes.
	offset, begin, first := 0, -1, 0
	for i, yes := range added {
		if yes && begin < 0 {
			begin = offset
			first = i + 1
		}
		if !yes && begin >= 0 {
			if err := emit(dst[begin:offset], first); err != nil {
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
		return emit(dst[begin:offset], first)
	}
	return ctx.Err()
}

func (w *worker) content(ctx context.Context, e object.ChangeEntry) (string, bool, error) {
	if e.TreeEntry.Mode == filemode.Submodule {
		return "Subproject commit " + e.TreeEntry.Hash.String() + "\n", false, nil
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
	return text.String(), false, nil
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
