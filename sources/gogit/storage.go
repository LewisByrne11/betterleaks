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

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

func (s *Source) open(ctx context.Context) (*git.Repository, *objectCache, func(), error) {
	// Reuse upstream discovery for bare repositories, gitdir files and commondir.
	// No worktree operations or object traversal run against this initial store.
	discovered, err := git.PlainOpen(s.RepoPath)
	if err != nil {
		return nil, nil, nil, err
	}
	initial := discovered.Storer.(*filesystem.Storage)
	base := initial.Filesystem()
	objectRoot := base.Root()
	common, commonErr := base.Open("commondir")
	if commonErr == nil {
		path, readErr := io.ReadAll(io.LimitReader(common, 4097))
		common.Close()
		if readErr != nil || len(path) > 4096 {
			initial.Close()
			return nil, nil, nil, errors.New("could not read bounded commondir path")
		}
		objectRoot = strings.TrimSpace(string(path))
		if !filepath.IsAbs(objectRoot) {
			objectRoot = filepath.Join(base.Root(), objectRoot)
		}
	} else if !errors.Is(commonErr, os.ErrNotExist) {
		initial.Close()
		return nil, nil, nil, commonErr
	}
	fs := readOnlyFS{Filesystem: base, objectRoot: objectRoot}
	if err := initial.Close(); err != nil {
		return nil, nil, nil, err
	}
	// Shallow scans need explicit boundary semantics; reject instead of claiming
	// a complete parent comparison when the available parents are incomplete.
	shallow, err := fs.Open("shallow")
	if err == nil {
		var b [1]byte
		n, readErr := shallow.Read(b[:])
		shallow.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, nil, nil, readErr
		}
		if n > 0 {
			return nil, nil, nil, errors.New("go-git history source does not yet support shallow repositories")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	budget := s.CacheMiB
	if budget == 0 {
		budget = 256
	}
	// Billy confines absolute paths to its root. A shared local clone's
	// alternates can lie outside the Git directory, so give alternate reads a
	// filesystem root while retaining the same read-only wrapper. Relative
	// alternates currently have different semantics in go-git; reject them.
	alternates, err := fs.Open("objects/info/alternates")
	if err == nil {
		data, readErr := io.ReadAll(io.LimitReader(alternates, 65537))
		alternates.Close()
		if readErr != nil {
			return nil, nil, nil, readErr
		}
		if len(data) > 65536 {
			return nil, nil, nil, errors.New("Git alternates file exceeds 64 KiB")
		}
		for _, path := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if path != "" && !filepath.IsAbs(path) {
				return nil, nil, nil, errors.New("go-git source does not yet support relative object alternates")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	root := filepath.VolumeName(objectRoot) + string(filepath.Separator)
	altFS := readOnlyFS{Filesystem: osfs.New(root), objectRoot: root}
	objects := &objectCache{cache.NewObjectLRU(cache.FileSize(budget) << 20)}
	store := filesystem.NewStorageWithOptions(fs, objects, filesystem.Options{LargeObjectThreshold: 1 << 20, AlternatesFS: altFS})
	reader := &objectStorage{Storage: store, objects: objects, ctx: ctx, limit: max(s.MaxBlobBytes, 64<<20)}
	diskBudget := s.DiskCacheMiB
	if diskBudget == 0 {
		diskBudget = 1024
	}
	if diskBudget > 0 {
		reader.disk = &diskObjectCache{segmentSize: int64(diskBudget) << 20 / 8}
	}
	if err := reader.openPacks(fs); err != nil {
		reader.closePacks()
		store.Close()
		return nil, nil, nil, err
	}
	r, err := git.Open(reader, nil)
	if err != nil {
		reader.closePacks()
		store.Close()
		return nil, nil, nil, err
	}
	return r, objects, func() { reader.closePacks(); store.Close() }, nil
}

// Read-only mappings remove random pack/index read syscalls. Resident mapped
// pages count toward RSS separately from the bounded object caches; the OS can
// reclaim clean pages. This wrapper preserves upstream's commondir/alternate
// routing and forbids repository mutations.
type readOnlyFS struct {
	billy.Filesystem
	objectRoot string
}

func (f readOnlyFS) Open(path string) (billy.File, error) {
	if strings.HasPrefix(filepath.ToSlash(path), "objects/pack/") && (strings.HasSuffix(path, ".idx") || strings.HasSuffix(path, ".rev") || strings.HasSuffix(path, ".pack")) {
		return osfs.New(f.objectRoot, osfs.WithMmap()).Open(path)
	}
	return f.Filesystem.Open(path)
}

// Git ignores dotfiles in ref directories (for example fixture .gitkeep files).
func (f readOnlyFS) ReadDir(path string) ([]os.DirEntry, error) {
	entries, err := f.Filesystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	// Object enumeration must not silently skip a linked fanout directory or
	// follow a loose object symlink/FIFO while looking for abandoned content.
	if clean == "objects" {
		for _, entry := range entries {
			if len(entry.Name()) == 2 {
				if _, err := hex.DecodeString(entry.Name()); err == nil && !entry.IsDir() {
					return nil, fmt.Errorf("loose object directory %q is not a directory", entry.Name())
				}
			}
		}
	} else if strings.HasPrefix(clean, "objects/") && len(clean) == len("objects/00") {
		prefix := strings.TrimPrefix(clean, "objects/")
		for _, entry := range entries {
			if plumbing.IsHash(prefix+entry.Name()) && !entry.Type().IsRegular() {
				return nil, fmt.Errorf("loose object %s is not a regular file", prefix+entry.Name())
			}
		}
	}
	if clean == "refs" || strings.HasPrefix(clean, "refs/") {
		visible := entries[:0]
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				visible = append(visible, entry)
			}
		}
		return visible, nil
	}
	return entries, nil
}

func (f readOnlyFS) OpenFile(path string, flag int, perm os.FileMode) (billy.File, error) {
	if flag != os.O_RDONLY {
		return nil, os.ErrPermission
	}
	return f.Open(path)
}
func (readOnlyFS) Create(string) (billy.File, error)           { return nil, os.ErrPermission }
func (readOnlyFS) TempFile(string, string) (billy.File, error) { return nil, os.ErrPermission }
func (readOnlyFS) Rename(string, string) error                 { return os.ErrPermission }
func (readOnlyFS) Remove(string) error                         { return os.ErrPermission }
func (readOnlyFS) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (readOnlyFS) Symlink(string, string) error                { return os.ErrPermission }
func (f readOnlyFS) Chroot(path string) (billy.Filesystem, error) {
	fs, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return readOnlyFS{Filesystem: fs, objectRoot: fs.Root()}, nil
}

// FSObject.Size reports content size, but its reader closure retains a Packfile
// and scanner buffers. Caching tiny commits this way can turn a 96 MiB cache
// into gigabytes. Keep materialized objects (including reconstructed delta
// bases) and let these storage handles go after each read. Their
// contents would still need decompression on every Reader call, even if cached.
type objectCache struct{ *cache.ObjectLRU }

func (c *objectCache) Put(obj plumbing.EncodedObject) {
	if _, lazy := obj.(*packfile.FSObject); lazy {
		return
	}
	if cache.FileSize(obj.Size()) > c.MaxSize {
		return
	}
	// Upstream's LRU calls Hash while holding its global mutex. Hashing a
	// reconstructed blob can scan megabytes and block every cache lookup.
	// Materialized objects are private to their decoder until this publication;
	// compute their memoized hash before entering the cache's critical section.
	obj.Hash()
	c.ObjectLRU.Put(obj)
}
