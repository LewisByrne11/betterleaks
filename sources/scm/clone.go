package scm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/betterleaks/betterleaks/v2/internal/urlredact"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// CloneAuthed fetches the branches, tags and notes of an HTTP(S) repository
// into a bare repository at dest, directly in Go. Host review refs such as
// refs/pull are excluded. It does not check out files, run credential helpers,
// recurse into submodules, or execute Git/SSH.
func CloneAuthed(ctx context.Context, remote, token, dest string) error {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("git clone requires an HTTP(S) URL without credentials, query or fragment; use Token for authentication")
	}
	if dest == "" {
		return errors.New("git clone destination is empty")
	}
	if token != "" && u.Scheme != "https" {
		return errors.New("git clone tokens require HTTPS")
	}
	var auth []client.Option
	if token != "" {
		auth = []client.Option{client.WithHTTPAuth(&githttp.BasicAuth{Username: "x-access-token", Password: token})}
	}
	if err := fetchPublishedRefs(ctx, remote, dest, auth); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git clone %s: %s", urlredact.Public(u), SanitizeOutput(err.Error(), token))
	}
	return nil
}

// fetchPublishedRefs keeps review-only history out of the download, rather than
// discarding its refs after fetching all of their objects.
func fetchPublishedRefs(ctx context.Context, remote, dest string, auth []client.Option) error {
	repo, err := git.PlainInit(dest, true)
	if err != nil {
		return err
	}
	defer repo.Storer.(*filesystem.Storage).Close()
	refs := []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "+refs/notes/*:refs/notes/*"}
	origin, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remote}, Fetch: refs})
	if err != nil {
		return err
	}
	advertised, err := origin.ListContext(ctx, &git.ListOptions{ClientOptions: auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	err = origin.FetchContext(ctx, &git.FetchOptions{RefSpecs: refs, ClientOptions: auth, Tags: plumbing.NoTags})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		// A remote containing only excluded refs is empty for this scan.
		return nil
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	for _, ref := range advertised {
		if ref.Name() != plumbing.HEAD {
			continue
		}
		if ref.Type() == plumbing.SymbolicReference {
			for _, spec := range refs {
				if spec.Match(ref.Target()) {
					return repo.Storer.SetReference(ref)
				}
			}
			return nil
		}
		if ref.Type() == plumbing.HashReference {
			// Preserve a detached HEAD only if the selected refs brought its
			// object with them; HEAD must not pull excluded history back in.
			if err := repo.Storer.HasEncodedObject(ref.Hash()); err != nil {
				if errors.Is(err, plumbing.ErrObjectNotFound) {
					return nil
				}
				return err
			}
			return repo.Storer.SetReference(ref)
		}
	}
	return nil
}

// CloneToTempDir clones a remote into a temporary directory, invokes fn with
// the cloned repo path, and removes the directory afterwards.
func CloneToTempDir(ctx context.Context, remote, token, tempDirPattern string, fn func(repoPath string) error) error {
	if tempDirPattern == "" {
		tempDirPattern = "betterleaks-git-*"
	}
	tmpDir, err := os.MkdirTemp("", tempDirPattern)
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := CloneAuthed(ctx, remote, token, tmpDir); err != nil {
		return err
	}
	return fn(tmpDir)
}

// SanitizeOutput redacts the token (and any URL-encoded form of it) from text.
// It also strips URL userinfo, query parameters, and fragments.
// Use it when wrapping transport errors.
func SanitizeOutput(text, token string) string {
	if text == "" {
		return text
	}
	if token != "" {
		text = strings.ReplaceAll(text, token, "***")
		if encoded := url.QueryEscape(token); encoded != token {
			text = strings.ReplaceAll(text, encoded, "***")
		}
	}
	return urlredact.Redact(text)
}
