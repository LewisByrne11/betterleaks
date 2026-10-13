package gogit

import (
	"context"
	"errors"
	"fmt"
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
	s := &Source{RepoPath: repo, Include: []string{"tag-messages"}, Exclude: []string{"history"}}
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
		s := &Source{RepoPath: path, All: true}
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
		s.Exclude = []string{"history"}
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
	s := &Source{RepoPath: repo, Include: []string{"reflog-messages"}, Exclude: []string{"history"}}
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
func TestObjectResourceCoverage(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "Objects")
	gitFixture(t, repo, "config", "user.email", "objects@example.test")
	write := func(name, value string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(value+"\n"), 0600))
	}
	write("file", "HISTORY_SECRET")
	gitFixture(t, repo, "add", "file")
	gitFixture(t, repo, "commit", "-qm", "root")
	root := gitFixture(t, repo, "rev-parse", "HEAD")
	rootBlob := gitFixture(t, repo, "rev-parse", "HEAD:file")
	rootObjectPath := filepath.Join(repo, ".git", "objects", rootBlob[:2], rootBlob[2:])
	rootObject, err := os.ReadFile(rootObjectPath)
	require.NoError(t, err)
	write("tag-only", "TAG_SECRET")
	gitFixture(t, repo, "add", "tag-only")
	gitFixture(t, repo, "commit", "-qm", "tag-only")
	gitFixture(t, repo, "tag", "-a", "saved", "-m", "tag message")
	gitFixture(t, repo, "reset", "--hard", root)
	write("abandoned", "ABANDONED_SECRET")
	gitFixture(t, repo, "add", "abandoned")
	gitFixture(t, repo, "commit", "-qm", "abandoned")
	gitFixture(t, repo, "reset", "--hard", root)
	write("tree-only", "TREE_SECRET")
	gitFixture(t, repo, "add", "tree-only")
	gitFixture(t, repo, "commit", "-qm", "tree-only")
	tree := gitFixture(t, repo, "rev-parse", "HEAD^{tree}")
	gitFixture(t, repo, "tag", "saved-tree", tree)
	gitFixture(t, repo, "reset", "--hard", root)
	gitFixture(t, repo, "notes", "add", "-m", "NOTE_SECRET")
	write("blob-tag", "BLOB_TAG_SECRET")
	blobTag := gitFixture(t, repo, "hash-object", "-w", "blob-tag")
	gitFixture(t, repo, "tag", "-a", "saved-blob", blobTag, "-m", "blob annotation")
	write("staged", "STAGED_SECRET")
	gitFixture(t, repo, "add", "staged")
	staged := gitFixture(t, repo, "rev-parse", ":staged")
	write("lost", "LOST_SECRET")
	lost := gitFixture(t, repo, "hash-object", "-w", "lost")
	// A gitlink is not an edge into this repository's object graph.
	gitFixture(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",submodule")
	gitFixture(t, repo, "reset", "-q", "HEAD", "staged")
	gitFixture(t, repo, "commit", "-qm", "gitlink")
	gitFixture(t, repo, "add", "staged")

	for _, packed := range []bool{false, true} {
		if packed {
			// Keep otherwise unreferenced blobs while packing, then remove the
			// temporary refs. Reflog-only commits are retained by repack itself.
			gitFixture(t, repo, "update-ref", "refs/keep/staged", staged)
			gitFixture(t, repo, "update-ref", "refs/keep/lost", lost)
			gitFixture(t, repo, "repack", "-ad")
			gitFixture(t, repo, "update-ref", "-d", "refs/keep/staged")
			gitFixture(t, repo, "update-ref", "-d", "refs/keep/lost")
			// The same blob exists loose and packed. It must still emit once.
			require.NoError(t, os.MkdirAll(filepath.Dir(rootObjectPath), 0700))
			require.NoError(t, os.WriteFile(rootObjectPath, rootObject, 0600))
		}
		reachable := make(map[string]bool)
		for _, id := range strings.Fields(gitFixture(t, repo, "rev-list", "--objects", "--all", "HEAD", "--no-object-names")) {
			reachable[id] = true
		}
		all, unreachable := make(map[string]bool), make(map[string]bool)
		for _, line := range strings.Split(gitFixture(t, repo, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)"), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == "blob" {
				all[fields[0]] = true
				if !reachable[fields[0]] {
					unreachable[fields[0]] = true
				}
			}
		}
		require.Len(t, unreachable, 3, "abandoned, staged, and unreferenced blobs; not notes or tag targets")
		for _, tc := range []struct {
			name             string
			include, exclude []string
			want             map[string]bool
		}{
			{"default", nil, nil, map[string]bool{}},
			{"add unreachable", []string{"unreachable-objects"}, nil, unreachable},
			{"unreachable only", []string{"unreachable-objects"}, []string{"history"}, unreachable},
			{"reflog and unreachable", []string{"unreachable-objects", "reflog-history"}, nil, unreachable},
			{"reflog without history", []string{"unreachable-objects", "reflog-history"}, []string{"history"}, unreachable},
			{"notes and unreachable", []string{"unreachable-objects", "notes"}, nil, unreachable},
			{"all objects", []string{"objects"}, nil, all},
			{"objects only", []string{"objects"}, []string{"history"}, all},
			{"both object selectors", []string{"objects", "unreachable-objects"}, []string{"history"}, all},
		} {
			t.Run(fmt.Sprintf("packed=%v/%s", packed, tc.name), func(t *testing.T) {
				t.Setenv("PATH", t.TempDir())
				s := &Source{RepoPath: repo, All: true, Include: tc.include, Exclude: tc.exclude}
				check := func() {
					got := make(map[string]bool)
					for _, f := range collect(t, s) {
						if f.Attr(sources.AttrResource) != sources.ResourceGitBlob {
							continue
						}
						id := f.Attr(AttrBlobSHA)
						require.False(t, got[id], "duplicate blob %s", id)
						got[id] = true
						require.Empty(t, f.Attr(sources.AttrPath))
						require.Empty(t, f.Attr(sources.AttrGitSHA))
					}
					require.Equal(t, tc.want, got)
				}
				check()
				// Selection can suppress every patch without changing object scope.
				s.All, s.Revision, s.MaxCount, s.Author = false, root, 1, "nobody"
				check()
			})
		}
		if packed {
			shared := filepath.Join(t.TempDir(), "shared.git")
			gitFixture(t, repo, "clone", "-q", "--mirror", "--shared", repo, shared)
			worktree := filepath.Join(t.TempDir(), "worktree")
			gitFixture(t, repo, "worktree", "add", "--detach", worktree)
			for _, path := range []string{shared, worktree} {
				t.Run(path, func(t *testing.T) {
					t.Setenv("PATH", t.TempDir())
					for resource, want := range map[string]map[string]bool{"objects": all, "unreachable-objects": unreachable} {
						got := make(map[string]bool)
						for _, f := range collect(t, &Source{RepoPath: path, Include: []string{resource}, Exclude: []string{"history"}}) {
							id := f.Attr(AttrBlobSHA)
							require.False(t, got[id])
							got[id] = true
						}
						require.Equal(t, want, got)
					}
				})
			}
		}
	}
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
			s := &Source{RepoPath: repo, All: true, MaxCount: tc.maxCount, Include: []string{"unreachable-objects"}}
			if tc.rejectAbandoned {
				s.Prefilter = func(attrs map[string]string) bool { return attrs[sources.AttrGitSHA] == abandoned }
			}
			baseline := collect(t, s)
			foundLoose := false
			for _, f := range baseline {
				foundLoose = foundLoose || (f.Attr(sources.AttrResource) == sources.ResourceGitBlob && f.Raw == content)
			}
			require.True(t, foundLoose, "baseline scans the complete abandoned loose blob")
			s.Include = []string{"unreachable-objects", "reflog-history"}
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

//nolint:exhaustruct // Fixtures specify only the options relevant to the scenario.
func TestMessageAndPatchScopes(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "-b", "main")
	gitFixture(t, repo, "config", "user.name", "Scope Test")
	gitFixture(t, repo, "config", "user.email", "scope@example.test")
	commit := func(name, date string) string {
		t.Setenv("GIT_COMMITTER_DATE", date)
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(name+"_CONTENT\n"), 0600))
		gitFixture(t, repo, "add", name)
		gitFixture(t, repo, "commit", "-qm", name+"_MESSAGE")
		return gitFixture(t, repo, "rev-parse", "HEAD")
	}
	base := commit("base", "2001-01-01T00:00:00Z")
	tagOnly := commit("tag", "2001-01-02T00:00:00Z")
	gitFixture(t, repo, "tag", "retained", tagOnly)
	gitFixture(t, repo, "reset", "--hard", base)
	abandoned := commit("abandoned", "2001-01-03T00:00:00Z")
	gitFixture(t, repo, "reset", "--hard", base)
	logs := filepath.Join(repo, ".git", "logs")
	require.NoError(t, os.RemoveAll(logs))
	require.NoError(t, os.Mkdir(logs, 0700))
	entry := abandoned + " " + base + " Test <test@example.test> 1700000000 +0000\treset\n"
	require.NoError(t, os.WriteFile(filepath.Join(logs, "HEAD"), []byte(entry), 0600))
	t.Setenv("PATH", t.TempDir())
	for _, limit := range []int{0, 1, 2, 3} {
		s := &Source{RepoPath: repo, All: true, MaxCount: limit, Include: []string{"commit-messages"}, Exclude: []string{"history"}}
		before := collect(t, s)
		s.Include = []string{"commit-messages", "reflog-history"}
		after := collect(t, s)
		for _, f := range before {
			require.Contains(t, after, f)
		}
		messages, patches := map[string]int{}, map[string]int{}
		for _, f := range after {
			switch f.Attr(sources.AttrResource) {
			case sources.ResourceGitCommitMessage:
				messages[f.Attr(sources.AttrGitSHA)]++
			case sources.ResourceGitPatchContent:
				patches[f.Attr(sources.AttrGitSHA)]++
			default:
				t.Fatalf("unexpected resource %s", f.Attr(sources.AttrResource))
			}
		}
		require.Equal(t, 1, messages[tagOnly])
		require.Zero(t, patches[tagOnly], "tag-only messages must not introduce file patches")
		if limit != 1 {
			require.Equal(t, 1, messages[base])
			require.Equal(t, 1, patches[base])
		}
		if limit == 0 || limit == 3 {
			require.Equal(t, 1, messages[abandoned])
			require.Equal(t, 1, patches[abandoned])
		} else {
			require.Zero(t, messages[abandoned])
			require.Zero(t, patches[abandoned])
		}
	}
}
