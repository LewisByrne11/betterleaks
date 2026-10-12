package gogit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/betterleaks/betterleaks/v2/sources"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// Only loose IDs are retained, not a reverse index of all historical blobs.
// Workers mark encountered versions before applying path filters so supplemental
// scans do not bypass those filters for the same version. The supplemental scan
// is independent of history selection.
type looseObjects struct {
	ids  map[plumbing.Hash]int // Immutable after discovery; lookups need no global lock.
	seen []atomic.Bool
}

func (l *looseObjects) encountered(id plumbing.Hash) {
	if l == nil {
		return
	}
	if index, ok := l.ids[id]; ok {
		l.seen[index].Store(true)
	}
}

func findLoose(ctx context.Context, store *objectStorage) (*looseObjects, error) {
	root := filepath.Join(store.Filesystem().(readOnlyFS).objectRoot, "objects")
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	loose := &looseObjects{ids: make(map[plumbing.Hash]int), seen: nil}
	for _, dir := range dirs {
		if len(dir.Name()) != 2 {
			continue
		}
		if _, err := hex.DecodeString(dir.Name()); err != nil {
			continue
		}
		if !dir.IsDir() {
			return nil, fmt.Errorf("loose object directory %q is not a directory", dir.Name())
		}
		f, err := os.Open(filepath.Join(root, dir.Name()))
		if err != nil {
			return nil, err
		}
		for {
			if err := ctx.Err(); err != nil {
				f.Close()
				return nil, err
			}
			entries, err := f.ReadDir(256)
			if err != nil && !errors.Is(err, io.EOF) {
				f.Close()
				return nil, err
			}
			for _, entry := range entries {
				name := dir.Name() + entry.Name()
				if len(name) != 40 {
					continue
				}
				if _, err := hex.DecodeString(name); err != nil {
					continue
				}
				if !entry.Type().IsRegular() {
					f.Close()
					return nil, fmt.Errorf("loose object %s is not a regular file", name)
				}
				if len(loose.ids) >= 1_000_000 {
					f.Close()
					return nil, errors.New("supplemental loose object inventory exceeds one million objects; repack or use --exclude=loose-objects")
				}
				id := plumbing.NewHash(name)
				if _, exists := loose.ids[id]; !exists {
					loose.ids[id] = len(loose.ids)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	if len(loose.ids) == 0 {
		return nil, nil
	}
	loose.seen = make([]atomic.Bool, len(loose.ids))
	return loose, nil
}

func (s *Source) looseBlobs(ctx context.Context, store *objectStorage, loose *looseObjects, yield sources.FragmentsFunc) error {
	if loose == nil {
		return nil
	}
	for id, index := range loose.ids {
		if loose.seen[index].Load() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		attrs := map[string]string{sources.AttrResource: sources.ResourceGitLooseBlob, AttrBlobSHA: id.String()}
		if s.Prefilter != nil && s.Prefilter(attrs) {
			continue
		}
		obj, err := store.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return fmt.Errorf("loose object %s: %w", id, err)
		}
		if obj.Type() != plumbing.BlobObject {
			continue
		}
		reader, err := obj.Reader()
		if err != nil {
			return err
		}
		// No filename is invented. File detects archives from their magic bytes.
		source := &sources.File{Path: "", Symlink: "", DetectArchive: true, StrictArchives: false, Content: reader, Attributes: attrs, MaxArchiveDepth: s.MaxArchiveDepth, Prefilter: s.Prefilter, Logger: s.Logger}
		err = source.Fragments(ctx, yield)
		closeErr := reader.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return ctx.Err()
}

// Notes are ordinary commits and trees under refs/notes/*. Scan each changed
// note version, including notes later removed. Their commit metadata describes
// the note edit; git.note_target identifies the annotated object, not its author.
func (s *Source) notes(ctx context.Context, r *git.Repository, loose *looseObjects, yield sources.FragmentsFunc) error {
	refs, err := r.Storer.IterReferences()
	if err != nil {
		return err
	}
	defer refs.Close()
	trees := new(treeCache)
	trees.r = r
	accept := s.selector()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref, err := refs.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(ref.Name().String(), "refs/notes/") {
			continue
		}
		selection := *s
		selection.All, selection.Revision = false, ref.Name().String()
		history, err := selection.history(ctx, r)
		if err != nil {
			return err
		}
		selected := 0
		for s.MaxCount == 0 || selected < s.MaxCount {
			commit, err := history.next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if !accept(commit) {
				continue
			}
			selected++
			changes, err := treeChanges(ctx, commit, trees)
			if err != nil {
				return err
			}
			for _, change := range changes {
				if change.To.Name == "" || change.To.TreeEntry.Mode == filemode.Submodule {
					continue
				}
				id := change.To.TreeEntry.Hash
				loose.encountered(id)
				attrs := commitAttributes(commit)
				attrs[sources.AttrResource] = sources.ResourceGitNote
				attrs["git.notes_ref"], attrs[AttrBlobSHA] = ref.Name().String(), id.String()
				target := strings.ReplaceAll(change.To.Name, "/", "")
				if len(target) == 40 {
					if _, err := hex.DecodeString(target); err == nil {
						attrs["git.note_target"] = target
					}
				}
				if s.Prefilter != nil && s.Prefilter(attrs) {
					continue
				}
				obj, err := r.Storer.EncodedObject(plumbing.BlobObject, id)
				if err != nil {
					return err
				}
				raw, err := readObjectString(ctx, obj, max(s.MaxBlobBytes, 64<<20))
				if err != nil {
					return err
				}
				if err := yield(sources.Fragment{Raw: raw, StartLine: 1, Attributes: attrs}, nil); err != nil {
					return err
				}
			}
		}
	}
}

// Lightweight tags have no message. Nested annotations are scanned too, but
// only the outer annotation has a tag ref suitable for generating a web link.
func (s *Source) tagMessages(ctx context.Context, r *git.Repository, yield sources.FragmentsFunc) error {
	refs, err := r.Tags()
	if err != nil {
		return err
	}
	defer refs.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref, err := refs.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		ref, err = storer.ResolveReference(r.Storer, ref.Name())
		if err != nil {
			return err
		}
		id := ref.Hash()
		for depth := 0; ; depth++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if depth == 32 {
				return errors.New("annotated tag nesting exceeds 32 levels")
			}
			obj, err := r.Storer.EncodedObject(plumbing.AnyObject, id)
			if err != nil {
				return fmt.Errorf("tag ref %s: %w", ref.Name(), err)
			}
			if obj.Type() != plumbing.TagObject {
				break
			}
			tag, err := object.DecodeTag(r.Storer, obj)
			if err != nil {
				return err
			}
			attrs := map[string]string{
				sources.AttrResource:       sources.ResourceGitTagMessage,
				sources.AttrGitTagName:     tag.Name,
				sources.AttrGitTaggerName:  tag.Tagger.Name,
				sources.AttrGitTaggerEmail: tag.Tagger.Email,
				"git.tagger_date":          tag.Tagger.When.UTC().Format(time.RFC3339),
				"git.tag_sha":              tag.Hash.String(), "git.tag_target": tag.Target.String(),
				"git.tag_source_ref": ref.Name().String(),
			}
			if depth == 0 {
				attrs[sources.AttrGitTagRef] = ref.Name().String()
				if s.RemoteURL != "" {
					attrs[sources.AttrGitRemoteURL] = s.RemoteURL
					attrs[sources.AttrGitPlatform] = s.Platform.String()
				}
			}
			if s.Prefilter == nil || !s.Prefilter(attrs) {
				if err := yield(sources.Fragment{Raw: tag.Message, StartLine: 1, Attributes: attrs}, nil); err != nil {
					return err
				}
			}
			if tag.TargetType != plumbing.TagObject {
				break
			}
			id = tag.Target
		}
	}
}
