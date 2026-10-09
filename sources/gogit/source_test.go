package gogit

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/stretchr/testify/require"
)

func gitFixture(t *testing.T, repo string, args ...string) string {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git fixture generator unavailable")
	}
	cmd := exec.Command(binary, append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}
func newHistory(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "First Author")
	gitFixture(t, repo, "config", "user.email", "first@example.test")
	write := func(name, text string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(text), 0644))
	}
	commit := func(message, date string) {
		gitFixture(t, repo, "add", ".")
		gitFixture(t, repo, "-c", "user.name="+message, "commit", "-qm", message, "--date", date)
	}
	write("file.txt", "first\nTOKEN_ROOT\nlast\n")
	commit("root", "2001-01-01T00:00:00Z")
	root := gitFixture(t, repo, "rev-parse", "HEAD")
	write("file.txt", "first\nTOKEN_CHANGED\nlast\n")
	commit("modify", "2001-01-02T00:00:00Z")
	gitFixture(t, repo, "mv", "file.txt", "renamed.txt")
	commit("rename", "2001-01-03T00:00:00Z")
	gitFixture(t, repo, "checkout", "-qb", "side", root)
	write("side.txt", "TOKEN_SIDE\n")
	commit("side", "2001-01-04T00:00:00Z")
	gitFixture(t, repo, "checkout", "-q", "main")
	gitFixture(t, repo, "merge", "--no-ff", "side", "-qm", "merge")
	// Local diff helpers and hooks must never execute during object reads.
	write(".gitattributes", "*.txt diff=evil\n")
	commit("attributes", "2001-01-05T00:00:00Z")
	marker := filepath.Join(repo, "executed")
	gitFixture(t, repo, "config", "diff.evil.textconv", "touch "+marker)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0755))
	return repo
}
func collect(t *testing.T, s *Source) []sources.Fragment {
	t.Helper()
	var mu sync.Mutex
	var fragments []sources.Fragment
	require.NoError(t, s.Fragments(t.Context(), func(f sources.Fragment, err error) error {
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		fragments = append(fragments, f)
		return nil
	}))
	return fragments
}
func textOccurrences(fs []sources.Fragment) map[string]string {
	result := map[string]string{}
	for _, f := range fs {
		if strings.Contains(f.Raw, "TOKEN_") {
			result[f.Raw] = fmt.Sprintf("%s:%d", f.Attr(sources.AttrPath), f.StartLine)
		}
	}
	return result
}

func TestSource(t *testing.T) {
	repo := newHistory(t)
	expected := map[string]string{"first\nTOKEN_ROOT\nlast\n": "file.txt:1", "TOKEN_CHANGED\n": "file.txt:2", "TOKEN_SIDE\n": "side.txt:1"}
	for _, packed := range []bool{false, true} {
		if packed {
			gitFixture(t, repo, "repack", "-ad")
		}
		t.Run(fmt.Sprintf("packed=%v", packed), func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			for _, workers := range []int{1, 4} {
				fs := collect(t, &Source{RepoPath: repo, Workers: workers})
				require.Equal(t, expected, textOccurrences(fs))
				for _, f := range fs {
					require.Len(t, f.Attr(sources.AttrGitSHA), 40)
					require.Len(t, f.Attr(AttrBlobSHA), 40)
					require.NotEmpty(t, f.Attr(sources.AttrGitAuthorName))
					require.NotEmpty(t, f.Attr(AttrCommitterDate))
					require.Equal(t, sources.ResourceGitPatchContent, f.Attr(sources.AttrResource))
				}
			}
			_, err := os.Stat(filepath.Join(repo, "executed"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}

	t.Run("alternates", func(t *testing.T) {
		shared := filepath.Join(t.TempDir(), "shared")
		gitFixture(t, repo, "clone", "-q", "--shared", repo, shared)
		require.Equal(t, expected, textOccurrences(collect(t, &Source{RepoPath: shared})))
	})
	t.Run("shallow", func(t *testing.T) {
		shallow := filepath.Join(t.TempDir(), "shallow")
		gitFixture(t, repo, "clone", "-q", "--depth=1", "file://"+repo, shallow)
		err := (&Source{RepoPath: shallow}).Fragments(t.Context(), func(sources.Fragment, error) error { t.Fatal("yield from unsupported shallow history"); return nil })
		require.ErrorContains(t, err, "shallow")
	})
	t.Run("cancel-slow-consumer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- (&Source{RepoPath: repo, Workers: 4}).Fragments(ctx, func(sources.Fragment, error) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("source did not yield")
		}
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("canceled source did not stop")
		}
	})
	t.Run("prefilter", func(t *testing.T) {
		fs := collect(t, &Source{RepoPath: repo, Prefilter: func(a map[string]string) bool { return a[sources.AttrPath] != "side.txt" }})
		require.Equal(t, map[string]string{"TOKEN_SIDE\n": "side.txt:1"}, textOccurrences(fs))
		// Rejected resources must not require content materialization.
		require.Empty(t, collect(t, &Source{RepoPath: repo, MaxBlobBytes: 1, Prefilter: func(map[string]string) bool { return true }}))
	})
	t.Run("author", func(t *testing.T) {
		fs := collect(t, &Source{RepoPath: repo, Author: "modify <"})
		require.Equal(t, map[string]string{"TOKEN_CHANGED\n": "file.txt:2"}, textOccurrences(fs))
	})
	t.Run("messages", func(t *testing.T) {
		fs := collect(t, &Source{RepoPath: repo, Include: []string{"commit-messages"}})
		messages := 0
		for _, f := range fs {
			if f.Attr(sources.AttrResource) == sources.ResourceGitCommitMessage {
				messages++
				require.Empty(t, f.Attr(sources.AttrPath))
			}
		}
		require.Equal(t, 6, messages)
	})
	t.Run("stop", func(t *testing.T) {
		stop := errors.New("consumer stopped")
		err := (&Source{RepoPath: repo, Workers: 4}).Fragments(t.Context(), func(sources.Fragment, error) error { return stop })
		require.ErrorIs(t, err, stop)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, (&Source{RepoPath: repo}).Fragments(ctx, func(sources.Fragment, error) error { t.Fatal("yield after cancellation"); return nil }), context.Canceled)
	})
	t.Run("limits", func(t *testing.T) {
		err := (&Source{RepoPath: repo, MaxBlobBytes: 1}).Fragments(t.Context(), func(sources.Fragment, error) error { return nil })
		require.ErrorContains(t, err, "diff input limit")
		require.Error(t, (&Source{RepoPath: repo, Author: "["}).Validate())
		require.Error(t, (&Source{RepoPath: repo, Include: []string{"reflogs"}}).Validate())
	})
	t.Run("bare-and-worktree", func(t *testing.T) {
		bare := filepath.Join(t.TempDir(), "bare")
		gitFixture(t, repo, "clone", "-q", "--bare", repo, bare)
		require.Equal(t, expected, textOccurrences(collect(t, &Source{RepoPath: bare})))
		linked := filepath.Join(t.TempDir(), "linked")
		gitFixture(t, repo, "worktree", "add", "--detach", linked, "HEAD")
		// The fixture's hook executed during Git's checkout, not during the scan.
		os.Remove(filepath.Join(repo, "executed"))
		require.Equal(t, expected, textOccurrences(collect(t, &Source{RepoPath: linked})))
		_, err := os.Stat(filepath.Join(repo, "executed"))
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestSelectionAndArchives(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "Test")
	gitFixture(t, repo, "config", "user.email", "test@example.test")
	commit := func(text, date string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, "file"), []byte(text), 0644))
		gitFixture(t, repo, "add", ".")
		gitFixture(t, repo, "-c", "user.name=Date Author", "commit", "-qm", text, "--date", date)
		// Set committer time independently of author time using a fixture process.
		cmd := exec.Command("git", "-C", repo, "commit", "--amend", "--no-edit", "--quiet")
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	commit("TOKEN_OLD\n", "2001-01-01T00:00:00Z")
	commit("TOKEN_NEW\n", "2001-01-03T00:00:00Z")
	date := time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC)
	require.Equal(t, map[string]string{"TOKEN_NEW\n": "file:1"}, textOccurrences(collect(t, &Source{RepoPath: repo, Since: &date})))
	require.Equal(t, map[string]string{"TOKEN_OLD\n": "file:1"}, textOccurrences(collect(t, &Source{RepoPath: repo, Until: &date})))
	require.Equal(t, map[string]string{"TOKEN_NEW\n": "file:1"}, textOccurrences(collect(t, &Source{RepoPath: repo, MaxCount: 1})))
	zipFile, err := os.Create(filepath.Join(repo, "archive.zip"))
	require.NoError(t, err)
	writer := zip.NewWriter(zipFile)
	inner, err := writer.Create("secret.txt")
	require.NoError(t, err)
	_, err = inner.Write([]byte("TOKEN_ARCHIVE\n"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, zipFile.Close())
	gitFixture(t, repo, "add", ".")
	gitFixture(t, repo, "commit", "-qm", "archive")
	fs := collect(t, &Source{RepoPath: repo, MaxCount: 1, MaxArchiveDepth: 1})
	require.Equal(t, map[string]string{"TOKEN_ARCHIVE\n": "archive.zip!secret.txt:1"}, textOccurrences(fs))
}

func TestSHA256(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "--object-format=sha256")
	gitFixture(t, repo, "config", "user.name", "Test")
	gitFixture(t, repo, "config", "user.email", "test@example.test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "secret"), []byte("TOKEN_SHA256\n"), 0644))
	gitFixture(t, repo, "add", ".")
	gitFixture(t, repo, "commit", "-qm", "root")
	gitFixture(t, repo, "repack", "-ad")
	fs := collect(t, &Source{RepoPath: repo})
	require.Len(t, fs, 1)
	require.Len(t, fs[0].Attr(AttrBlobSHA), 64)
	require.Len(t, fs[0].Attr(sources.AttrGitSHA), 64)
}

// Test the edit script rather than mirroring the diff implementation: emitted
// text must refer to the stated destination lines, and everything not emitted
// must be an ordered subsequence of the parent. No possible addition is lost.
func TestLineAdditions(t *testing.T) {
	split := func(text string) []string {
		if text == "" {
			return nil
		}
		lines := strings.SplitAfter(text, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	check := func(src, dst string) {
		t.Helper()
		current := split(dst)
		added := make([]bool, len(current))
		previousEnd := 0
		err := lineAdditions(t.Context(), src, dst, func(text string, line int) error {
			start := line - 1
			n := len(split(text))
			require.GreaterOrEqual(t, start, previousEnd)
			require.LessOrEqual(t, start+n, len(current))
			// Split preserving every byte, including a final unterminated line.
			expected := strings.Join(current[start:start+n], " ")
			actual := strings.Join(split(text), " ")
			require.Equal(t, expected, actual)
			for i := start; i < start+n; i++ {
				added[i] = true
			}
			previousEnd = start + n
			return nil
		})
		require.NoError(t, err)
		old := split(src)
		at := 0
		for i, text := range current {
			if added[i] {
				continue
			}
			for at < len(old) && old[at] != text {
				at++
			}
			require.Less(t, at, len(old), "retained line not found in ordered parent")
			at++
		}
	}
	rng := rand.New(rand.NewPCG(13, 27))
	words := []string{"one\n", "two\n", "two\n", "\n", "unicode-世界\n", "crlf\r\n"}
	for range 500 {
		var src, dst strings.Builder
		for range rng.IntN(60) {
			src.WriteString(words[rng.IntN(len(words))])
		}
		for range rng.IntN(60) {
			dst.WriteString(words[rng.IntN(len(words))])
		}
		check(src.String(), dst.String())
		check(strings.TrimSuffix(src.String(), "\n"), strings.TrimSuffix(dst.String(), "\n"))
	}
	// Large complete rewrites must not spend quadratic time on unmatched lines.
	var old, current strings.Builder
	for i := range 70000 {
		fmt.Fprintf(&old, "old-%d\n", i)
		fmt.Fprintf(&current, "new-%d\n", i)
	}
	check(old.String(), current.String())
	require.ErrorContains(t, lineAdditions(t.Context(), strings.Repeat("x\n", 1_000_000), "", func(string, int) error { t.Fatal("oversized line input yielded"); return nil }), "token limit")
	stop := errors.New("stop")
	require.ErrorIs(t, lineAdditions(t.Context(), "old\n", "new\n", func(string, int) error { return stop }), stop)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, lineAdditions(ctx, "old\n", "new\n", func(string, int) error { t.Fatal("emit after cancel"); return nil }), context.Canceled)
}

func FuzzLineAdditions(f *testing.F) {
	f.Add("old\ncommon\n", "new\ncommon\n")
	f.Add("", "no final newline")
	f.Add("a\nb\na\n", "b\na\nb\n")
	f.Fuzz(func(t *testing.T, old, current string) {
		if len(old)+len(current) > 8192 {
			t.Skip()
		}
		lines := func(text string) []string {
			if text == "" {
				return nil
			}
			parts := strings.SplitAfter(text, "\n")
			if parts[len(parts)-1] == "" {
				parts = parts[:len(parts)-1]
			}
			return parts
		}
		a, b := lines(old), lines(current)
		added := make([]bool, len(b))
		last := 0
		require.NoError(t, lineAdditions(t.Context(), old, current, func(text string, line int) error {
			first := line - 1
			n := len(lines(text))
			require.GreaterOrEqual(t, first, last)
			require.LessOrEqual(t, first+n, len(b))
			require.Equal(t, strings.Join(b[first:first+n], ""), text)
			for i := first; i < first+n; i++ {
				added[i] = true
			}
			last = first + n
			return nil
		}))
		at := 0
		for i, text := range b {
			if added[i] {
				continue
			}
			for at < len(a) && a[at] != text {
				at++
			}
			require.Less(t, at, len(a))
			at++
		}
	})
}
