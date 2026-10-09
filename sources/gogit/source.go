// Package gogit scans local committed additions using go-git's object reader.
// It never invokes Git, hooks, filters, diff helpers, or a network transport.
package gogit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"runtime"
	"slices"
	"time"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/betterleaks/betterleaks/v2/sources/scm"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"golang.org/x/sync/errgroup"
)

// Source scans HEAD's reachable history by default. Merges are compared against
// their first parent, matching Betterleaks' existing Git source. Include can add commit-messages.
// Diff boundaries and rename choices can differ from Git's xdiff implementation.
// All settings are immutable while Fragments runs; calls can run independently.
type Source struct {
	RepoPath string
	Revision string
	All      bool
	// NoMergePatches matches plain git log -p's suppression of merge patches.
	NoMergePatches bool
	// Since and Until filter committer time inclusively, without pruning ancestors
	// based on timestamps. Non-monotonic history therefore remains traversable.
	Since, Until *time.Time
	// Author and Committer are Go regular expressions over "Name <email>".
	Author, Committer string
	MaxCount          int
	// Workers defaults to min(GOMAXPROCS, 4). Work queues and caches are bounded.
	Workers int
	// CacheMiB defaults to 96; the parsed-tree cache has a separate 32 MiB budget.
	CacheMiB int
	// MaxBlobBytes defaults to 64 MiB. Exceeding it returns an incomplete-scan
	// error, rather than silently omitting content. It bounds diff inputs, not
	// go-git's internal delta reconstruction or the process's complete RSS.
	MaxBlobBytes    int64
	Include         []string
	Prefilter       sources.PrefilterFunc
	MaxArchiveDepth int
	Logger          *slog.Logger
	Platform        scm.Platform
	RemoteURL       string
}

var _ sources.Source = (*Source)(nil)

func (s *Source) Validate() error {
	if s.RepoPath == "" {
		return errors.New("go-git source requires a local repository path")
	}
	if s.Workers < 0 || s.Workers > 16 || s.CacheMiB < 0 || s.MaxCount < 0 || s.MaxBlobBytes < 0 {
		return errors.New("invalid go-git worker, cache, count, or blob limit")
	}
	if s.All && s.Revision != "" {
		return errors.New("go-git All and Revision are mutually exclusive")
	}
	if s.Since != nil && s.Until != nil && s.Until.Before(*s.Since) {
		return errors.New("until must not precede since")
	}
	for _, pattern := range []string{s.Author, s.Committer} {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("invalid identity filter: %w", err)
		}
	}
	for _, name := range s.Include {
		if name != sources.GitResourceTypeCommitMessages {
			return fmt.Errorf("go-git does not support resource %q; supported: commit-messages", name)
		}
	}
	return nil
}

func (s *Source) Fragments(ctx context.Context, yield sources.FragmentsFunc) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r, cleanup, err := s.open()
	if err != nil {
		return err
	}
	defer cleanup()
	opts := &git.LogOptions{All: s.All, Order: git.LogOrderCommitterTime, Since: s.Since, Until: s.Until}
	if !s.All {
		revision := s.Revision
		if revision == "" {
			revision = "HEAD"
		}
		hash, e := r.ResolveRevision(plumbing.Revision(revision))
		if e != nil {
			return fmt.Errorf("resolve revision %q: %w", revision, e)
		}
		opts.From = *hash
	}
	iter, err := r.Log(opts)
	if err != nil {
		return err
	}
	defer iter.Close()
	author, _ := regexp.Compile(s.Author)
	committer, _ := regexp.Compile(s.Committer)
	workers := s.Workers
	if workers == 0 {
		workers = min(max(runtime.GOMAXPROCS(0), 1), 4)
	}
	trees := &treeCache{r: r}
	maxBlob := s.MaxBlobBytes
	if maxBlob == 0 {
		maxBlob = 64 << 20
	}
	// The producer owns traversal; at most one queued commit per worker.
	group, workCtx := errgroup.WithContext(ctx)
	jobs := make(chan *object.Commit, workers)
	for range workers {
		group.Go(func() error {
			w := worker{source: s, trees: trees, maxBlob: maxBlob}
			for {
				select {
				case <-workCtx.Done():
					return workCtx.Err()
				case c, ok := <-jobs:
					if !ok {
						return nil
					}
					if err := w.commit(workCtx, c, yield); err != nil {
						return fmt.Errorf("commit %s: %w", c.Hash, err)
					}
				}
			}
		})
	}
	group.Go(func() error {
		defer close(jobs)
		selected := 0
		for s.MaxCount == 0 || selected < s.MaxCount {
			if err := workCtx.Err(); err != nil {
				return err
			}
			c, e := iter.Next()
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return e
			}
			if !author.MatchString(c.Author.Name+" <"+c.Author.Email+">") || !committer.MatchString(c.Committer.Name+" <"+c.Committer.Email+">") {
				continue
			}
			selected++
			select {
			case jobs <- c:
			case <-workCtx.Done():
				return workCtx.Err()
			}
		}
		return nil
	})
	return group.Wait()
}

type worker struct {
	source  *Source
	trees   *treeCache
	maxBlob int64
	buffer  [32 << 10]byte
}

func (w *worker) commit(ctx context.Context, c *object.Commit, yield sources.FragmentsFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attrs := map[string]string{
		sources.AttrGitSHA: c.Hash.String(), sources.AttrGitAuthorName: c.Author.Name, sources.AttrGitAuthorEmail: c.Author.Email,
		sources.AttrGitDate: c.Author.When.UTC().Format(time.RFC3339), sources.AttrGitMessage: c.Message,
		AttrCommitterName: c.Committer.Name, AttrCommitterEmail: c.Committer.Email, AttrCommitterDate: c.Committer.When.UTC().Format(time.RFC3339),
	}
	if w.source.RemoteURL != "" {
		attrs[sources.AttrGitRemoteURL] = w.source.RemoteURL
		attrs[sources.AttrGitPlatform] = w.source.Platform.String()
	}
	if slices.Contains(w.source.Include, sources.GitResourceTypeCommitMessages) {
		messageAttrs := maps.Clone(attrs)
		messageAttrs[sources.AttrResource] = sources.ResourceGitCommitMessage
		if w.source.Prefilter == nil || !w.source.Prefilter(messageAttrs) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(sources.Fragment{Raw: c.Message, StartLine: 1, Attributes: messageAttrs}, nil); err != nil {
				return err
			}
		}
	}
	if w.source.NoMergePatches && len(c.ParentHashes) > 1 {
		return nil
	}
	changes, err := treeChanges(ctx, c, w.trees)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if change.To.Name == "" {
			continue
		}
		fileAttrs := maps.Clone(attrs)
		fileAttrs[sources.AttrPath] = change.To.Name
		fileAttrs[sources.AttrResource] = sources.ResourceGitPatchContent
		fileAttrs[AttrBlobSHA] = change.To.TreeEntry.Hash.String()
		if w.source.Prefilter != nil && w.source.Prefilter(fileAttrs) {
			continue
		}
		emit := func(raw string, line int) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return yield(sources.Fragment{Raw: raw, StartLine: line, Attributes: fileAttrs}, nil)
		}
		if err := w.change(ctx, change, fileAttrs, emit, yield); err != nil {
			return fmt.Errorf("path %q: %w", change.To.Name, err)
		}
	}
	return nil
}

const (
	AttrBlobSHA        = "git.blob_sha"
	AttrCommitterName  = "git.committer_name"
	AttrCommitterEmail = "git.committer_email"
	AttrCommitterDate  = "git.committer_date"
)
