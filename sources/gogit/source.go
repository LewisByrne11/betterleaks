// Package gogit scans committed additions using go-git's object reader.
// It never invokes Git, hooks, filters, or diff helpers. URL scans fetch an
// HTTP(S) mirror; local repository scans do not use a network transport.
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
// their first parent. Include adds resources to the default history scan;
// Exclude removes resources. Conflicts and empty selections are rejected.
// Object scans read full blobs without path or commit attribution.
// Diff boundaries and rename choices can differ from Git's xdiff implementation.
// Traversal retains one visited object ID per discovered commit, not every
// decoded commit. Worker queues and object/tree caches have separate bounds.
// All settings are immutable while Fragments runs; calls can run independently.
type Source struct {
	RepoPath string
	// URL scans a temporary HTTP(S) mirror. Mutually exclusive with RepoPath.
	URL, Token string
	Revision   string
	All        bool
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
	MaxBlobBytes int64
	// Include and Exclude accept history, merges, objects, unreachable-objects, commit-messages,
	// notes, tag-messages, reflog-messages, and reflog-history.
	Include, Exclude []string
	Prefilter        sources.PrefilterFunc
	MaxArchiveDepth  int
	Logger           *slog.Logger
	// Platform and RemoteURL control finding links. By default they are resolved
	// from the local repository's fetch remote without invoking Git. NoPlatform
	// disables links; RemoteURL overrides the configured remote.
	Platform  scm.Platform
	RemoteURL string
}

var _ sources.Source = (*Source)(nil)

func (s *Source) Validate() error {
	if (s.RepoPath == "") == (s.URL == "") {
		return errors.New("git source requires either a local repository path or an HTTP(S) URL")
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
	_, err := s.resources()
	return err
}

func (s *Source) resources() (map[string]bool, error) {
	selected := map[string]bool{
		sources.GitResourceTypeHistory:            true,
		sources.GitResourceTypeMerges:             true,
		sources.GitResourceTypeObjects:            false,
		sources.GitResourceTypeUnreachableObjects: false,
		sources.GitResourceTypeCommitMessages:     false,
		sources.GitResourceTypeNotes:              false,
		sources.GitResourceTypeTagMessages:        false,
		sources.GitResourceTypeReflogMessages:     false,
		sources.GitResourceTypeReflogHistory:      false,
	}
	for _, list := range [][]string{s.Include, s.Exclude} {
		for _, name := range list {
			if _, known := selected[name]; !known {
				return nil, fmt.Errorf("unsupported Git resource %q; supported: history, merges, objects, unreachable-objects, commit-messages, notes, tag-messages, reflog-messages, reflog-history", name)
			}
		}
	}
	for _, name := range s.Include {
		if slices.Contains(s.Exclude, name) {
			return nil, fmt.Errorf("git resource %q cannot be both included and excluded", name)
		}
		selected[name] = true
	}
	for _, name := range s.Exclude {
		selected[name] = false
	}
	// Merge patches are part of history, not an independent traversal.
	if !selected[sources.GitResourceTypeHistory] && !selected[sources.GitResourceTypeReflogHistory] {
		if slices.Contains(s.Include, sources.GitResourceTypeMerges) {
			return nil, errors.New("including merges requires history or reflog-history")
		}
		selected[sources.GitResourceTypeMerges] = false
	}
	for _, enabled := range selected {
		if enabled {
			return selected, nil
		}
	}
	return nil, errors.New("git resource selection leaves nothing to scan")
}

func (s *Source) Fragments(ctx context.Context, yield sources.FragmentsFunc) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.URL != "" {
		return scm.CloneToTempDir(ctx, s.URL, s.Token, "betterleaks-git-*", scm.CloneOptions{Mirror: true, Bare: false, SingleBranch: false, Depth: 0}, func(path string) error {
			local := *s
			local.RepoPath, local.URL, local.Token = path, "", ""
			return local.Fragments(ctx, yield)
		})
	}
	r, _, cleanup, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	// Resolve once per scan without mutating caller-owned settings; the same
	// Source can be scanned concurrently.
	resolved := *s
	resources, _ := s.resources() // Validate already checked the selection.
	if resources[sources.GitResourceTypeHistory] || resources[sources.GitResourceTypeCommitMessages] || resources[sources.GitResourceTypeReflogHistory] || resources[sources.GitResourceTypeTagMessages] {
		resolved.Platform, resolved.RemoteURL = s.resolveRemote(r)
	}
	store := r.Storer.(*objectStorage)
	var ordinary commitResources
	if resources[sources.GitResourceTypeHistory] {
		ordinary |= scanPatches
	}
	if resources[sources.GitResourceTypeCommitMessages] {
		ordinary |= scanMessages
	}
	var walk *history
	if ordinary != 0 {
		walk, err = resolved.history(ctx, r, ordinary)
		if err != nil {
			return err
		}
		if err := resolved.commits(ctx, r, walk, false, yield); err != nil {
			return err
		}
	} else if resources[sources.GitResourceTypeReflogHistory] {
		walk = &history{r: r, seen: make(map[plumbing.Hash]commitResources), scope: 0, selected: 0, frontier: nil, sequence: 0}
	}
	if resources[sources.GitResourceTypeReflogHistory] || resources[sources.GitResourceTypeReflogMessages] {
		var tips *history
		if resources[sources.GitResourceTypeReflogHistory] {
			tips = walk
			// A count limit can leave queued ordinary commits. Drop that queue;
			// only reflog roots may request patches when history is excluded.
			walk.frontier = nil
			walk.scope = scanPatches | ordinary&scanMessages
		}
		if err := resolved.reflogs(ctx, store, tips, resources[sources.GitResourceTypeReflogMessages], yield); err != nil {
			return err
		}
	}
	if resources[sources.GitResourceTypeReflogHistory] {
		// Messages-only commits may still need reflog patches, even after the
		// count budget is spent. Their metadata is revisited but messages are
		// never emitted twice, and no file tree is compared twice.
		revisit := ordinary == scanMessages
		if err := resolved.commits(ctx, r, walk, revisit, yield); err != nil {
			return err
		}
	}
	if resources[sources.GitResourceTypeTagMessages] {
		if err := resolved.tagMessages(ctx, r, yield); err != nil {
			return err
		}
	}
	if resources[sources.GitResourceTypeNotes] {
		if err := resolved.notes(ctx, r, yield); err != nil {
			return err
		}
	}
	if resources[sources.GitResourceTypeObjects] || resources[sources.GitResourceTypeUnreachableObjects] {
		return resolved.blobs(ctx, r, !resources[sources.GitResourceTypeObjects], yield)
	}
	return ctx.Err()
}

func (s *Source) commits(ctx context.Context, r *git.Repository, history *history, revisit bool, yield sources.FragmentsFunc) error {
	accept := s.selector()
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
	type job struct {
		commit    *object.Commit
		resources commitResources
	}
	jobs := make(chan job, workers)
	for range workers {
		group.Go(func() error {
			var differ lineDiffer
			w := worker{source: s, trees: trees, store: r.Storer.(*objectStorage), maxBlob: maxBlob, patches: patches, differ: differ}
			for {
				select {
				case <-workCtx.Done():
					return workCtx.Err()
				case c, ok := <-jobs:
					if !ok {
						return nil
					}
					if err := w.commit(workCtx, c.commit, c.resources, yield); err != nil {
						return fmt.Errorf("commit %s: %w", c.commit.Hash, err)
					}
				}
			}
		})
	}
	group.Go(func() error {
		defer close(jobs)
		for revisit || s.MaxCount == 0 || history.selected < s.MaxCount {
			if err := workCtx.Err(); err != nil {
				return err
			}
			c, resources, e := history.next(workCtx)
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return e
			}
			if !accept(c) {
				continue
			}
			if history.seen[c.Hash]&commitSelected == 0 {
				if s.MaxCount > 0 && history.selected >= s.MaxCount {
					continue
				}
				history.selected++
				history.seen[c.Hash] |= commitSelected
			}
			select {
			case jobs <- job{c, resources}:
			case <-workCtx.Done():
				return workCtx.Err()
			}
		}
		return nil
	})
	err := group.Wait()
	if s.Logger != nil {
		s.Logger.Debug("git text patch cache", "hits", patches.hits, "misses", patches.misses, "entries", len(patches.entries), "bytes", patches.bytes)
	}
	return err
}

func (s *Source) selector() func(*object.Commit) bool {
	author, _ := regexp.Compile(s.Author)
	committer, _ := regexp.Compile(s.Committer)
	return func(c *object.Commit) bool {
		return !(s.Since != nil && c.Committer.When.Before(*s.Since)) && !(s.Until != nil && c.Committer.When.After(*s.Until)) &&
			author.MatchString(c.Author.Name+" <"+c.Author.Email+">") && committer.MatchString(c.Committer.Name+" <"+c.Committer.Email+">")
	}
}

func commitAttributes(c *object.Commit) map[string]string {
	return map[string]string{
		sources.AttrGitSHA: c.Hash.String(), sources.AttrGitAuthorName: c.Author.Name, sources.AttrGitAuthorEmail: c.Author.Email,
		sources.AttrGitDate: c.Author.When.UTC().Format(time.RFC3339), sources.AttrGitMessage: c.Message,
		AttrCommitterName: c.Committer.Name, AttrCommitterEmail: c.Committer.Email, AttrCommitterDate: c.Committer.When.UTC().Format(time.RFC3339),
	}
}

type worker struct {
	source  *Source
	trees   *treeCache
	store   *objectStorage
	maxBlob int64
	differ  lineDiffer
	patches *patchCache
}

func (w *worker) commit(ctx context.Context, c *object.Commit, resources commitResources, yield sources.FragmentsFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attrs := commitAttributes(c)
	if w.source.RemoteURL != "" {
		attrs[sources.AttrGitRemoteURL] = w.source.RemoteURL
		attrs[sources.AttrGitPlatform] = w.source.Platform.String()
	}
	if resources&scanMessages != 0 {
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
	if resources&scanPatches == 0 || (slices.Contains(w.source.Exclude, sources.GitResourceTypeMerges) && len(c.ParentHashes) > 1) {
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
