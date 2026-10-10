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
	"github.com/go-git/go-git/v6/plumbing/object"
	"golang.org/x/sync/errgroup"
)

// Source scans HEAD's reachable history by default. Merges are compared against
// their first parent, matching Betterleaks' existing Git source. Include can add commit-messages.
// Diff boundaries and rename choices can differ from Git's xdiff implementation.
// Traversal retains one visited object ID per discovered commit, not every
// decoded commit. Worker queues and object/tree caches have separate bounds.
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
	// Workers defaults to min(GOMAXPROCS, 8). Work queues and caches are bounded.
	Workers int
	// CacheMiB defaults to 256 and limits cached materialized content bytes,
	// excluding object overhead. Parsed trees have a separate 32 MiB budget.
	CacheMiB int
	// DiskCacheMiB bounds a private temporary cache of large reconstructed
	// objects. Zero uses 1024 MiB; -1 disables it. The file is created lazily
	// in os.TempDir and removed when the scan ends. Cache I/O failure falls
	// back to object reconstruction without dropping scan content.
	DiskCacheMiB int
	// MaxBlobBytes defaults to 64 MiB. Exceeding it returns an incomplete-scan
	// error, rather than silently omitting content. It bounds diff inputs, not
	// go-git's internal delta reconstruction or the process's complete RSS.
	MaxBlobBytes    int64
	Include         []string
	Prefilter       sources.PrefilterFunc
	MaxArchiveDepth int
	Logger          *slog.Logger
	// Platform and RemoteURL control finding links. By default they are resolved
	// from the local repository's fetch remote without invoking Git. NoPlatform
	// disables links; RemoteURL overrides the configured remote.
	Platform  scm.Platform
	RemoteURL string
}

var _ sources.Source = (*Source)(nil)

func (s *Source) Validate() error {
	if s.RepoPath == "" {
		return errors.New("go-git source requires a local repository path")
	}
	if s.Workers < 0 || s.Workers > 16 || s.CacheMiB < 0 || s.DiskCacheMiB < -1 || s.DiskCacheMiB > 4096 || s.MaxCount < 0 || s.MaxBlobBytes < 0 {
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
	r, _, cleanup, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	// Resolve once per scan without mutating caller-owned settings; the same
	// Source can be scanned concurrently.
	resolved := *s
	resolved.Platform, resolved.RemoteURL = s.resolveRemote(r)
	history, err := s.history(ctx, r)
	if err != nil {
		return err
	}
	author, _ := regexp.Compile(s.Author)
	committer, _ := regexp.Compile(s.Committer)
	workers := s.Workers
	if workers == 0 {
		workers = min(max(runtime.GOMAXPROCS(0), 1), 8)
	}
	trees := &treeCache{r: r}
	patches := &patchCache{}
	maxBlob := s.MaxBlobBytes
	if maxBlob == 0 {
		maxBlob = 64 << 20
	}
	// The producer owns traversal; at most one queued commit per worker.
	group, workCtx := errgroup.WithContext(ctx)
	jobs := make(chan *object.Commit, workers)
	for range workers {
		group.Go(func() error {
			w := worker{source: &resolved, trees: trees, store: r.Storer.(*objectStorage), maxBlob: maxBlob, patches: patches}
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
			c, e := history.next(workCtx)
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return e
			}
			if (s.Since != nil && c.Committer.When.Before(*s.Since)) || (s.Until != nil && c.Committer.When.After(*s.Until)) {
				continue
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
	err = group.Wait()
	if s.Logger != nil {
		s.Logger.Debug("git text patch cache", "hits", patches.hits, "misses", patches.misses, "entries", len(patches.entries), "bytes", patches.bytes)
	}
	return err
}

type worker struct {
	source  *Source
	trees   *treeCache
	store   *objectStorage
	maxBlob int64
	differ  lineDiffer
	patches *patchCache
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
