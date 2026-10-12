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
	"github.com/go-git/go-git/v6/plumbing/client"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

// CloneOptions selects the refs to fetch. Scans use mirrors without a worktree.
type CloneOptions struct {
	Bare, Mirror, SingleBranch bool
	Depth                      int
}

// CloneAuthed fetches HTTP(S) Git objects directly in Go. It does not check out
// files, run credential helpers, recurse into submodules, or execute Git/SSH.
func CloneAuthed(ctx context.Context, remote, token, dest string, opts CloneOptions) error {
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
	options := new(git.CloneOptions)
	options.URL, options.Bare, options.Mirror = remote, opts.Bare || opts.Mirror, opts.Mirror
	options.NoCheckout, options.AllowEmptyRepo = true, true
	options.SingleBranch, options.Depth = opts.SingleBranch, opts.Depth
	if token != "" {
		options.ClientOptions = []client.Option{client.WithHTTPAuth(&githttp.BasicAuth{Username: "x-access-token", Password: token})}
	}
	repo, err := git.PlainCloneContext(ctx, dest, options)
	if repo != nil {
		defer repo.Storer.(*filesystem.Storage).Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git clone %s: %s", urlredact.Public(u), SanitizeOutput(err.Error(), token))
	}
	return nil
}

// CloneToTempDir clones a remote into a temporary directory, invokes fn with
// the cloned repo path, and removes the directory afterwards.
func CloneToTempDir(ctx context.Context, remote, token, tempDirPattern string, opts CloneOptions, fn func(repoPath string) error) error {
	if tempDirPattern == "" {
		tempDirPattern = "betterleaks-git-*"
	}
	tmpDir, err := os.MkdirTemp("", tempDirPattern)
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := CloneAuthed(ctx, remote, token, tmpDir, opts); err != nil {
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
