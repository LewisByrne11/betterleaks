package gogit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

func (s *Source) open() (*git.Repository, func(), error) {
	// Reuse upstream discovery for bare repositories, gitdir files and commondir.
	// No worktree operations or object traversal run against this initial store.
	discovered, err := git.PlainOpen(s.RepoPath)
	if err != nil {
		return nil, nil, err
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
			return nil, nil, errors.New("could not read bounded commondir path")
		}
		objectRoot = strings.TrimSpace(string(path))
		if !filepath.IsAbs(objectRoot) {
			objectRoot = filepath.Join(base.Root(), objectRoot)
		}
	} else if !errors.Is(commonErr, os.ErrNotExist) {
		initial.Close()
		return nil, nil, commonErr
	}
	fs := readOnlyFS{Filesystem: base, objectRoot: objectRoot}
	if err := initial.Close(); err != nil {
		return nil, nil, err
	}
	// Shallow scans need explicit boundary semantics; reject instead of claiming
	// a complete parent comparison when the available parents are incomplete.
	shallow, err := fs.Open("shallow")
	if err == nil {
		var b [1]byte
		n, readErr := shallow.Read(b[:])
		shallow.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, nil, readErr
		}
		if n > 0 {
			return nil, nil, errors.New("go-git history source does not yet support shallow repositories")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	budget := s.CacheMiB
	if budget == 0 {
		budget = 96
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
			return nil, nil, readErr
		}
		if len(data) > 65536 {
			return nil, nil, errors.New("Git alternates file exceeds 64 KiB")
		}
		for _, path := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if path != "" && !filepath.IsAbs(path) {
				return nil, nil, errors.New("go-git source does not yet support relative object alternates")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	root := filepath.VolumeName(objectRoot) + string(filepath.Separator)
	altFS := readOnlyFS{Filesystem: osfs.New(root), objectRoot: root}
	store := filesystem.NewStorageWithOptions(fs, cache.NewObjectLRU(cache.FileSize(budget)<<20), filesystem.Options{LargeObjectThreshold: 1 << 20, AlternatesFS: altFS})
	r, err := git.Open(store, nil)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return r, func() { store.Close() }, nil
}

// Mapping only indexes removes random index-read syscalls without making the
// entire pack's resident pages part of RSS. This wrapper preserves upstream's
// commondir/alternate routing, and forbids storage mutations.
type readOnlyFS struct {
	billy.Filesystem
	objectRoot string
}

func (f readOnlyFS) Open(path string) (billy.File, error) {
	if strings.HasPrefix(filepath.ToSlash(path), "objects/pack/") && (strings.HasSuffix(path, ".idx") || strings.HasSuffix(path, ".rev")) {
		return osfs.New(f.objectRoot, osfs.WithMmap()).Open(path)
	}
	return f.Filesystem.Open(path)
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
