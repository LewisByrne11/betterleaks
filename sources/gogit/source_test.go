package gogit

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
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

func TestHistoricalPathNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture contains a literal backslash filename")
	}
	repo := newHistory(t)
	old := strings.Repeat("common line\n", 100) + "TOKEN_BEFORE\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, "before"), []byte(old), 0600))
	gitFixture(t, repo, "add", "before")
	gitFixture(t, repo, "commit", "-qm", "before historical rename")
	name := "\\"
	gitFixture(t, repo, "mv", "before", name)
	require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(strings.Repeat("common line\n", 100)+"TOKEN_AFTER\n"), 0600))
	gitFixture(t, repo, "add", "--", name)
	gitFixture(t, repo, "commit", "-qm", "historical rename")
	fs := collect(t, &Source{RepoPath: repo, MaxCount: 1})
	require.Len(t, fs, 1)
	require.Equal(t, name, fs[0].Attr(sources.AttrPath))
	require.Equal(t, "TOKEN_AFTER\n", fs[0].Raw)
	require.Equal(t, 101, fs[0].StartLine)
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
	// Shared tokens cross the Unicode surrogate boundary as well. The final
	// insertion must retain its original coordinate after matching all of them.
	var additions []string
	require.NoError(t, lineAdditions(t.Context(), old.String(), old.String()+"secret\n", func(raw string, line int) error {
		require.Equal(t, 70001, line)
		additions = append(additions, raw)
		return nil
	}))
	require.Equal(t, []string{"secret\n"}, additions)
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

// A timestamp walk can encounter a shared first parent while another merge
// parent remains queued. Stopping at the shared parent loses that entire branch.
func TestAllHistoryCoverage(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := t.TempDir()
			gitFixture(t, repo, "init", "-q", "-b", "main", "--object-format="+format)
			gitFixture(t, repo, "config", "user.name", "Test")
			gitFixture(t, repo, "config", "user.email", "test@example.test")
			require.NoError(t, os.WriteFile(filepath.Join(repo, "secret"), []byte("TOKEN_HISTORY\n"), 0644))
			gitFixture(t, repo, "add", ".")
			tree := gitFixture(t, repo, "write-tree")
			commit := func(message, date string, parents ...string) string {
				t.Helper()
				args := []string{"-C", repo, "commit-tree", tree, "-m", message}
				for _, parent := range parents {
					args = append(args, "-p", parent)
				}
				cmd := exec.Command("git", args...)
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
				raw, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", raw)
				return strings.TrimSpace(string(raw))
			}
			root := commit("root", "2000-01-01T00:00:00Z")
			shared := commit("shared", "2030-01-01T00:00:00Z", root)
			side := commit("side", "2010-01-01T00:00:00Z", root)
			merge := commit("merge", "2020-01-01T00:00:00Z", shared, side)
			orphan := commit("tag-only", "2005-01-01T00:00:00Z")
			gitFixture(t, repo, "update-ref", "refs/heads/main", shared)
			gitFixture(t, repo, "update-ref", "refs/heads/other", merge)
			gitFixture(t, repo, "update-ref", "refs/remotes/origin/main", shared)
			gitFixture(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
			gitFixture(t, repo, "tag", "-a", "release", orphan, "-m", "tag")
			gitFixture(t, repo, "tag", "-a", "nested", "release", "-m", "nested tag")
			gitFixture(t, repo, "repack", "-ad")
			expected := strings.Fields(gitFixture(t, repo, "rev-list", "--all"))
			messages := func(s *Source) map[string]int {
				s.Include = []string{sources.GitResourceTypeCommitMessages}
				result := map[string]int{}
				for _, f := range collect(t, s) {
					if f.Attr(sources.AttrResource) == sources.ResourceGitCommitMessage {
						result[f.Attr(sources.AttrGitSHA)]++
					}
				}
				return result
			}
			actual := messages(&Source{RepoPath: repo, All: true})
			require.Len(t, actual, len(expected))
			for _, sha := range expected {
				require.Equal(t, 1, actual[sha], "missing or repeated commit %s", sha)
			}
			require.Contains(t, actual, side)
			require.Contains(t, actual, orphan)
			limited := messages(&Source{RepoPath: repo, All: true, MaxCount: 1})
			require.Equal(t, map[string]int{shared: 1}, limited)
			since := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
			selected := messages(&Source{RepoPath: repo, All: true, Since: &since})
			require.Equal(t, map[string]int{side: 1, merge: 1, shared: 1}, selected)
			// Invalid refs must not be quietly skipped as upstream's All iterator does.
			missing := strings.Repeat("1", len(root))
			gitFixture(t, repo, "update-ref", "refs/heads/broken", root)
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "broken"), []byte(missing+"\n"), 0644))
			err := (&Source{RepoPath: repo, All: true}).Fragments(t.Context(), func(sources.Fragment, error) error { return nil })
			require.ErrorContains(t, err, "broken")
			gitFixture(t, repo, "update-ref", "-d", "refs/heads/broken")
			// A syntactically valid commit can still name an unavailable parent.
			// Report the gap rather than treating it as the end of that ref's walk.
			raw := "tree " + tree + "\nparent " + missing + "\nauthor Test <test@example.test> 0 +0000\ncommitter Test <test@example.test> 0 +0000\n\nincomplete history\n"
			write := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "-w", "--stdin")
			write.Stdin = strings.NewReader(raw)
			written, err := write.CombinedOutput()
			require.NoError(t, err, "%s", written)
			gitFixture(t, repo, "update-ref", "refs/heads/incomplete", strings.TrimSpace(string(written)))
			err = (&Source{RepoPath: repo, All: true}).Fragments(t.Context(), func(sources.Fragment, error) error { return nil })
			require.ErrorContains(t, err, "parent "+missing)
		})
	}
}

func TestObjectCacheDoesNotRetainPackReaders(t *testing.T) {
	c := &objectCache{cache.NewObjectLRU(1024)}
	id := plumbing.NewHash(strings.Repeat("a", 40))
	lazy := packfile.NewFSObject(id, plumbing.CommitObject, 0, 1, nil, nil, nil, "", c)
	c.Put(lazy)
	_, found := c.Get(id)
	require.False(t, found)
	memory := plumbing.NewMemoryObject(plumbing.FromObjectFormat(format.SHA1))
	memory.SetType(plumbing.BlobObject)
	writer, err := memory.Writer()
	require.NoError(t, err)
	_, err = writer.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	c.Put(memory)
	actual, found := c.Get(memory.Hash())
	require.True(t, found)
	require.Same(t, memory, actual)
	c.Clear()
	_, found = c.Get(memory.Hash())
	require.False(t, found)
}

func TestCachedTextDeltaBase(t *testing.T) {
	base := &storedObject{kind: plumbing.BlobObject, raw: "abc"}
	reader, err := base.Reader()
	require.NoError(t, err)
	seeker, ok := reader.(io.Seeker)
	require.True(t, ok)
	_, err = seeker.Seek(1, io.SeekStart)
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "bc", string(raw))
	require.NoError(t, reader.Close())
	_, err = base.Writer()
	require.Error(t, err)
	// Copy all three source bytes, then append one literal byte. The source's
	// cached representation must remain usable by go-git's delta reconstruction.
	target := plumbing.NewMemoryObject(plumbing.FromObjectFormat(format.SHA1))
	require.NoError(t, packfile.ApplyDelta(target, base, bytes.NewBuffer([]byte{3, 4, 0x90, 3, 1, 'd'})))
	result, err := target.Reader()
	require.NoError(t, err)
	defer result.Close()
	raw, err = io.ReadAll(result)
	require.NoError(t, err)
	require.Equal(t, "abcd", string(raw))
	require.Equal(t, plumbing.BlobObject, base.Type())
	require.Equal(t, int64(3), base.Size())
}

type slowHashObject struct {
	plumbing.EncodedObject
	entered, release chan struct{}
	once             sync.Once
}

func (o *slowHashObject) Hash() plumbing.Hash {
	o.once.Do(func() { close(o.entered); <-o.release })
	return o.EncodedObject.Hash()
}

func TestObjectHashDoesNotBlockCache(t *testing.T) {
	c := &objectCache{cache.NewObjectLRU(1024)}
	seed := &storedObject{kind: plumbing.BlobObject, id: plumbing.NewHash(strings.Repeat("1", 40)), raw: "seed"}
	c.Put(seed)
	obj := &slowHashObject{EncodedObject: &storedObject{kind: plumbing.BlobObject, id: plumbing.NewHash(strings.Repeat("2", 40)), raw: "slow"}, entered: make(chan struct{}), release: make(chan struct{})}
	put := make(chan struct{})
	defer func() { close(obj.release); <-put }()
	go func() { c.Put(obj); close(put) }()
	<-obj.entered
	lookup := make(chan bool, 1)
	go func() { _, ok := c.Get(seed.id); lookup <- ok }()
	select {
	case ok := <-lookup:
		require.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("hashing an unpublished object blocked unrelated cache lookup")
	}
}

// Set BETTERLEAKS_GIT_BENCH_REPO to an absolute fixture path. No detector or
// tree traversal runs here, making graph retention and startup costs visible.
func BenchmarkHistory(b *testing.B) {
	path := os.Getenv("BETTERLEAKS_GIT_BENCH_REPO")
	if path == "" {
		b.Skip("set BETTERLEAKS_GIT_BENCH_REPO")
	}
	b.ReportAllocs()
	for range b.N {
		s := &Source{RepoPath: path, All: true}
		r, _, close, err := s.open(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		h, err := s.history(b.Context(), r)
		if err != nil {
			close()
			b.Fatal(err)
		}
		n, frontier := 0, 0
		for {
			_, err := h.next(b.Context())
			if err == io.EOF {
				break
			}
			if err != nil {
				close()
				b.Fatal(err)
			}
			n++
			frontier = max(frontier, len(h.frontier))
		}
		close()
		b.ReportMetric(float64(n), "commits/op")
		b.ReportMetric(float64(frontier), "frontier/op")
	}
}
