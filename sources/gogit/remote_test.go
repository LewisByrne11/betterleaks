package gogit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/betterleaks/betterleaks/v2/sources/scm"
	"github.com/stretchr/testify/require"
)

func TestRemoteSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		remotes  map[string]string
		branch   string
		detached bool
		rewrite  bool
		explicit string
		platform scm.Platform
		want     string
	}{
		{name: "no remote"},
		{name: "origin", remotes: map[string]string{"origin": "https://github.com/org/origin.git"}, want: "https://github.com/org/origin"},
		{name: "sole remote", remotes: map[string]string{"upstream": "https://github.com/org/upstream.git"}, want: "https://github.com/org/upstream"},
		{name: "origin preferred", remotes: map[string]string{"origin": "https://github.com/org/origin.git", "upstream": "https://github.com/org/upstream.git"}, want: "https://github.com/org/origin"},
		{name: "branch preferred", remotes: map[string]string{"origin": "https://github.com/org/origin.git", "upstream": "https://github.com/org/upstream.git"}, branch: "upstream", want: "https://github.com/org/upstream"},
		{name: "detached uses origin", remotes: map[string]string{"origin": "https://github.com/org/origin.git", "upstream": "https://github.com/org/upstream.git"}, branch: "upstream", detached: true, want: "https://github.com/org/origin"},
		{name: "ambiguous remotes", remotes: map[string]string{"first": "https://github.com/org/first.git", "second": "https://github.com/org/second.git"}},
		{name: "local branch remote", remotes: map[string]string{"origin": "https://github.com/org/origin.git"}, branch: "."},
		{name: "local clone", remotes: map[string]string{"origin": "/local/repo"}},
		{name: "URL rewrite", remotes: map[string]string{"origin": "gh:org/repo.git"}, rewrite: true, want: "https://github.com/org/repo"},
		{name: "explicit remote", remotes: map[string]string{"origin": "https://github.com/org/origin.git"}, explicit: "git@github.com:org/override.git", want: "https://github.com/org/override"},
		{name: "disabled", remotes: map[string]string{"origin": "https://github.com/org/origin.git"}, explicit: "https://github.com/org/override.git", platform: scm.NoPlatform},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			gitFixture(t, repo, "init", "-q", "-b", "main")
			gitFixture(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-qm", "root")
			for name, remote := range tc.remotes {
				gitFixture(t, repo, "remote", "add", name, remote)
			}
			if tc.branch != "" {
				gitFixture(t, repo, "config", "branch.main.remote", tc.branch)
			}
			if tc.detached {
				gitFixture(t, repo, "checkout", "--detach", "-q")
			}
			if tc.rewrite {
				gitFixture(t, repo, "config", "url.https://github.com/.insteadOf", "gh:")
			}
			if tc.explicit == "" && tc.platform != scm.NoPlatform {
				_, remote := sources.ResolveRemote(t.Context(), tc.platform, repo)
				require.Equal(t, tc.want, remote, "Git subprocess remote selection")
			}
			t.Setenv("PATH", t.TempDir())
			s := &Source{RepoPath: repo, RemoteURL: tc.explicit, Platform: tc.platform}
			r, _, cleanup, err := s.open(t.Context())
			require.NoError(t, err)
			defer cleanup()
			platform, remote := s.resolveRemote(r)
			require.Equal(t, tc.want, remote)
			if tc.want != "" {
				require.Equal(t, scm.GitHubPlatform, platform)
			}
		})
	}
}

func TestRemoteMetadataRepositoryLayouts(t *testing.T) {
	repo := t.TempDir()
	gitFixture(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("TOKEN_ROOT\n"), 0600))
	gitFixture(t, repo, "add", ".")
	gitFixture(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "root")
	remote := "https://github.com/org/repo"
	gitFixture(t, repo, "remote", "add", "origin", remote+".git")
	worktree := filepath.Join(t.TempDir(), "worktree")
	gitFixture(t, repo, "worktree", "add", "-q", "-b", "linked", worktree)
	bare := filepath.Join(t.TempDir(), "bare.git")
	gitFixture(t, repo, "clone", "--bare", "-q", repo, bare)
	gitFixture(t, bare, "remote", "set-url", "origin", remote+".git")
	t.Setenv("PATH", t.TempDir())
	for name, path := range map[string]string{"ordinary": repo, "worktree": worktree, "bare": bare} {
		t.Run(name, func(t *testing.T) {
			s := &Source{RepoPath: path, Include: []string{sources.GitResourceTypeCommitMessages}}
			fragments := collect(t, s)
			require.Len(t, fragments, 2)
			for _, f := range fragments {
				require.Equal(t, remote, f.Attr(sources.AttrGitRemoteURL))
				require.Equal(t, "github", f.Attr(sources.AttrGitPlatform))
			}
			require.Empty(t, s.RemoteURL, "resolution must not mutate caller settings")
			require.Equal(t, scm.UnknownPlatform, s.Platform)
		})
	}
}
