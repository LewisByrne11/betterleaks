package gogit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/stretchr/testify/require"
)

//nolint:exhaustruct // Fixtures specify only the options relevant to the scenario.
func TestTagMessages(t *testing.T) {
	repo := newHistory(t)
	gitFixture(t, repo, "tag", "lightweight")
	gitFixture(t, repo, "tag", "-a", "inner", "-m", "INNER_SECRET")
	inner := gitFixture(t, repo, "rev-parse", "inner")
	gitFixture(t, repo, "tag", "-a", "outer", "inner", "-m", "OUTER_SECRET")
	gitFixture(t, repo, "tag", "-d", "inner")
	gitFixture(t, repo, "gc")
	t.Setenv("PATH", t.TempDir())
	s := &Source{RepoPath: repo, Include: []string{"tag-messages"}, Exclude: []string{"history", "loose-objects"}}
	fs := collect(t, s)
	require.Len(t, fs, 2)
	for _, f := range fs {
		require.Equal(t, sources.ResourceGitTagMessage, f.Attr(sources.AttrResource))
		require.Empty(t, f.Attr(sources.AttrGitSHA), "a tag ID is not a commit ID")
		require.NotEmpty(t, f.Attr(sources.AttrGitTaggerEmail))
		require.NotEmpty(t, f.Attr("git.tagger_date"))
		require.Equal(t, 1, f.StartLine)
		require.Equal(t, "refs/tags/outer", f.Attr("git.tag_source_ref"))
		switch f.Raw {
		case "INNER_SECRET\n":
			require.Equal(t, inner, f.Attr("git.tag_sha"))
			require.Empty(t, f.Attr(sources.AttrGitTagRef))
		case "OUTER_SECRET\n":
			require.Equal(t, inner, f.Attr("git.tag_target"))
			require.Equal(t, "refs/tags/outer", f.Attr(sources.AttrGitTagRef))
		default:
			t.Fatalf("unexpected annotation: %q", f.Raw)
		}
	}
	s.Prefilter = func(a map[string]string) bool { return a[sources.AttrGitTagName] == "outer" }
	fs = collect(t, s)
	require.Len(t, fs, 1)
	require.Equal(t, "INNER_SECRET\n", fs[0].Raw)
	stop := errors.New("stop")
	require.ErrorIs(t, s.Fragments(t.Context(), func(sources.Fragment, error) error { return stop }), stop)
}

//nolint:exhaustruct // Fixtures specify only the options relevant to the scenario.
func TestReflogHistoryAndWorktrees(t *testing.T) {
	repo := newHistory(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	gitFixture(t, repo, "worktree", "add", "--detach", worktree)
	base := gitFixture(t, worktree, "rev-parse", "HEAD")
	file := filepath.Join(worktree, "abandoned")
	require.NoError(t, os.WriteFile(file, []byte("ABANDONED_SECRET\n"), 0600))
	gitFixture(t, worktree, "add", "abandoned")
	gitFixture(t, worktree, "commit", "-qm", "ABANDONED_MESSAGE")
	abandoned := gitFixture(t, worktree, "rev-parse", "HEAD")
	gitFixture(t, worktree, "reset", "--hard", base)
	// Retain only the reset record: the discarded commit occurs solely in its
	// old-ID field. It is reachable from no ref, and gc packs it via its reflog.
	logPath := filepath.Join(repo, ".git", "worktrees", "linked", "logs", "HEAD")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.NoError(t, os.WriteFile(logPath, []byte(lines[len(lines)-1]+"\n"), 0600))
	gitFixture(t, repo, "gc")
	t.Setenv("PATH", t.TempDir())
	for _, path := range []string{repo, worktree} {
		s := &Source{RepoPath: path, All: true, Exclude: []string{"loose-objects"}}
		for _, f := range collect(t, s) {
			require.NotContains(t, f.Raw, "ABANDONED")
		}
		s.Include = []string{"reflog-history", "commit-messages", "reflog-messages"}
		fs := collect(t, s)
		patches, messages, resets := 0, 0, 0
		for _, f := range fs {
			if f.Raw == "ABANDONED_SECRET\n" {
				patches++
				require.Equal(t, abandoned, f.Attr(sources.AttrGitSHA))
				require.Equal(t, "abandoned", f.Attr(sources.AttrPath))
			}
			if f.Raw == "ABANDONED_MESSAGE\n" {
				messages++
			}
			if f.Attr("git.reflog_old_sha") == abandoned {
				resets++
				require.Equal(t, "HEAD", f.Attr(sources.AttrGitReflogRef))
				require.Equal(t, "worktrees/linked/logs/HEAD", filepath.ToSlash(f.Attr("git.reflog_log")))
				require.Empty(t, f.Attr(sources.AttrGitSHA))
				require.NotEmpty(t, f.Attr(sources.AttrGitReflogActorEmail))
			}
		}
		require.Equal(t, 1, patches, "each commit is scanned once across all roots")
		require.Equal(t, 1, messages)
		require.Equal(t, 1, resets, "linked logs are not scanned twice")
		s.Exclude = []string{"history", "loose-objects"}
		s.Include = []string{"reflog-history", "merges"}
		found := false
		for _, f := range collect(t, s) {
			found = found || f.Raw == "ABANDONED_SECRET\n"
		}
		require.True(t, found)
	}
}

//nolint:exhaustruct // Fixtures specify only the options relevant to the scenario.
func TestReflogErrorsAndCancellation(t *testing.T) {
	repo := newHistory(t)
	logs := filepath.Join(repo, ".git", "logs")
	require.NoError(t, os.RemoveAll(logs))
	require.NoError(t, os.Mkdir(logs, 0700))
	logPath := filepath.Join(logs, "HEAD")
	zero := strings.Repeat("0", 40)
	missing := strings.Repeat("1", 40)
	entry := zero + " " + missing + " Actor <actor@example.test> 1700000000 -0500\tREFLOG_SECRET\n"
	t.Setenv("PATH", t.TempDir())
	s := &Source{RepoPath: repo, Include: []string{"reflog-messages"}, Exclude: []string{"history", "loose-objects"}}
	require.Empty(t, collect(t, s), "absent journals are valid")
	require.NoError(t, os.WriteFile(logPath, []byte(entry), 0600))
	fs := collect(t, s)
	require.Len(t, fs, 1)
	require.Equal(t, "REFLOG_SECRET", fs[0].Raw)
	require.Equal(t, "2023-11-14T22:13:20Z", fs[0].Attr("git.reflog_date"))
	stop := errors.New("consumer stopped")
	require.ErrorIs(t, s.Fragments(t.Context(), func(sources.Fragment, error) error { return stop }), stop)
	ctx, cancel := context.WithCancel(t.Context())
	err := s.Fragments(ctx, func(sources.Fragment, error) error { cancel(); return nil })
	require.ErrorIs(t, err, context.Canceled)
	s.Include = []string{"reflog-history"}
	require.ErrorContains(t, s.Fragments(t.Context(), func(sources.Fragment, error) error { return nil }), "read history tip")
	s.Include = []string{"reflog-messages"}
	for _, bad := range []string{
		"SECRET_INVALID\n", strings.TrimSuffix(entry, "\n"),
		strings.Replace(entry, missing, "INVALID_SECRET", 1),
		strings.Replace(entry, "-0500", "+9999", 1),
		strings.Repeat("x", 1<<20) + "\n",
	} {
		require.NoError(t, os.WriteFile(logPath, []byte(bad), 0600))
		err := s.Fragments(t.Context(), func(sources.Fragment, error) error { return nil })
		require.Error(t, err)
		require.NotContains(t, err.Error(), "SECRET")
	}
	require.NoError(t, os.Remove(logPath))
	outside := filepath.Join(t.TempDir(), "journal")
	require.NoError(t, os.WriteFile(outside, []byte(entry), 0600))
	require.NoError(t, os.Symlink(outside, logPath))
	require.ErrorContains(t, s.Fragments(t.Context(), func(sources.Fragment, error) error { return nil }), "not a regular file")
}

func FuzzParseReflog(f *testing.F) {
	f.Add(strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " Test <test@example.test> 1700000000 +0000\tmessage")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		e, err := parseReflog(raw)
		if err == nil {
			require.True(t, e.when.Year() >= 0 && e.when.Year() <= 9999)
		}
	})
}

//nolint:exhaustruct // Fixtures specify only the options relevant to the scenario.
func TestReflogHistoryIsAdditive(t *testing.T) {
	repo := newHistory(t)
	base := gitFixture(t, repo, "rev-parse", "HEAD")
	file := filepath.Join(repo, "renamed.txt")
	content := "first\nTOKEN_CHANGED\nlast\nABANDONED_ADDITION\n"
	require.NoError(t, os.WriteFile(file, []byte(content), 0600))
	gitFixture(t, repo, "add", "renamed.txt")
	t.Setenv("GIT_COMMITTER_DATE", "2030-01-01T00:00:00Z")
	gitFixture(t, repo, "commit", "-qm", "abandoned")
	abandoned := gitFixture(t, repo, "rev-parse", "HEAD")
	gitFixture(t, repo, "reset", "--hard", base)
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name            string
		maxCount        int
		rejectAbandoned bool
	}{
		{"defaults", 0, false},
		{"prefilter", 0, true},
		{"count limit", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Source{RepoPath: repo, All: true, MaxCount: tc.maxCount}
			if tc.rejectAbandoned {
				s.Prefilter = func(attrs map[string]string) bool { return attrs[sources.AttrGitSHA] == abandoned }
			}
			baseline := collect(t, s)
			foundLoose := false
			for _, f := range baseline {
				foundLoose = foundLoose || (f.Attr(sources.AttrResource) == sources.ResourceGitLooseBlob && f.Raw == content)
			}
			require.True(t, foundLoose, "baseline scans the complete abandoned loose blob")
			s.Include = []string{"reflog-history"}
			expanded := collect(t, s)
			for _, f := range baseline {
				require.Contains(t, expanded, f, "adding reflog history must retain each default fragment")
			}
			additions := 0
			for _, f := range expanded {
				if f.Attr(sources.AttrGitSHA) == abandoned {
					additions++
					require.Equal(t, "ABANDONED_ADDITION\n", f.Raw)
				}
			}
			if !tc.rejectAbandoned && tc.maxCount == 0 {
				require.Equal(t, 1, additions)
			} else {
				require.Zero(t, additions)
			}
		})
	}
}
