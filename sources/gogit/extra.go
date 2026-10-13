package gogit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/betterleaks/betterleaks/v2/sources"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// Notes are ordinary commits and trees under refs/notes/*. Scan each changed
// note version, including notes later removed. Their commit metadata describes
// the note edit; git.note_target identifies the annotated object, not its author.
func (s *Source) notes(ctx context.Context, r *git.Repository, yield sources.FragmentsFunc) error {
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
		history, err := selection.history(ctx, r, scanPatches)
		if err != nil {
			return err
		}
		selected := 0
		for s.MaxCount == 0 || selected < s.MaxCount {
			commit, _, err := history.next(ctx)
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
