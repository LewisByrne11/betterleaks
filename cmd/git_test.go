package cmd

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGitBinaryArchiveMembersIncluded(t *testing.T) {
	repo := t.TempDir()
	gitCmdT(t, repo, "init", "-q")
	gitCmdT(t, repo, "config", "user.name", "Archive Test")
	gitCmdT(t, repo, "config", "user.email", "archive@example.test")
	makeZip := func(files map[string][]byte) []byte {
		t.Helper()
		var out bytes.Buffer
		writer := zip.NewWriter(&out)
		for name, content := range files {
			entry, err := writer.Create(name)
			require.NoError(t, err)
			_, err = entry.Write(content)
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		return out.Bytes()
	}
	nested := makeZip(map[string][]byte{
		"text.bin":  []byte("SECRET_NESTED_TEXT\n"),
		"image.txt": []byte("\x89PNG\r\n\x1a\n\x00SECRET_NESTED_BINARY\n"),
	})
	archive := makeZip(map[string][]byte{
		"text.txt":   []byte("SECRET_TEXT\n"),
		"data.bin":   []byte("\x00SECRET_BINARY\x00\n"),
		"nested.zip": nested,
	})
	require.NoError(t, os.WriteFile(filepath.Join(repo, "secrets.zip"), archive, 0600))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "-qm", "archive")
	configPath := writeTestConfig(t, "[[rules]]\nid='archive-secret'\nregex='SECRET_[A-Z_]+'\n")
	for _, engine := range []string{"git", "go-git"} {
		t.Run(engine, func(t *testing.T) {
			root, stdout := newTestCLI(t)
			root.SetArgs([]string{"git", repo, "--git-engine=" + engine, "--config", configPath, "--no-banner", "--exit-code=0", "--output=-"})
			require.NoError(t, root.Execute())
			_, findings := decodeScanJSON(t, stdout.Bytes())
			var values []string
			for _, finding := range findings {
				values = append(values, finding.Match.Value)
			}
			require.ElementsMatch(t, []string{"SECRET_TEXT", "SECRET_NESTED_TEXT", "SECRET_BINARY", "SECRET_NESTED_BINARY"}, values)
		})
	}
}

func TestGitFindingURLs(t *testing.T) {
	repo := t.TempDir()
	gitCmdT(t, repo, "init", "-q", "-b", "main")
	gitCmdT(t, repo, "config", "user.name", "URL Test")
	gitCmdT(t, repo, "config", "user.email", "url@example.test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "secret file.txt"), []byte("ordinary\nSECRET_FILE\n"), 0600))
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("member.txt")
	require.NoError(t, err)
	_, err = entry.Write([]byte("ordinary\nSECRET_ARCHIVE\n"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(filepath.Join(repo, "archive.zip"), archive.Bytes(), 0600))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "-qm", "SECRET_MESSAGE")
	sha := gitCmdT(t, repo, "rev-parse", "HEAD")
	gitCmdT(t, repo, "remote", "add", "origin", "https://github.com/org/repo.git")
	configPath := writeTestConfig(t, "[[rules]]\nid='url-secret'\nregex='SECRET_[A-Z_]+'\n")
	for _, tc := range []struct {
		name, remote, platform, base, commitPath string
	}{
		{"GitHub HTTPS", "https://user:fixture-password@github.com/org/repo.git?token=fixture-query#fixture-fragment", "", "https://github.com/org/repo", "/commit/"},
		{"GitLab SSH", "git@gitlab.com:group/sub/repo.git", "", "https://gitlab.com/group/sub/repo", "/-/commit/"},
		{"self hosted", "ssh://git@git.example.test:2222/group/repo.git", "gitlab", "https://git.example.test/group/repo", "/-/commit/"},
		{"disabled", "https://github.com/org/repo.git", "none", "", ""},
		{"unknown host", "https://example.test/org/repo.git", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitCmdT(t, repo, "remote", "set-url", "origin", tc.remote)
			for _, engine := range []string{"git", "go-git"} {
				t.Run(engine, func(t *testing.T) {
					if engine == "go-git" {
						t.Setenv("PATH", t.TempDir())
					}
					root, stdout := newTestCLI(t)
					args := []string{"git", repo, "--git-engine=" + engine, "--include=commit-messages", "--config", configPath, "--no-banner", "--exit-code=0", "--output=-"}
					if tc.platform != "" {
						args = append(args, "--platform="+tc.platform)
					}
					root.SetArgs(args)
					require.NoError(t, root.Execute())
					_, findings := decodeScanJSON(t, stdout.Bytes())
					require.Len(t, findings, 3)
					for _, finding := range findings {
						want := ""
						if tc.base != "" {
							switch finding.Match.Value {
							case "SECRET_FILE":
								want = tc.base + "/blob/" + sha + "/secret%20file.txt#L2"
							case "SECRET_ARCHIVE":
								want = tc.base + "/blob/" + sha + "/archive.zip"
							case "SECRET_MESSAGE":
								want = tc.base + tc.commitPath + sha
							default:
								t.Fatalf("unexpected fixture finding %q", finding.Match.Value)
							}
						}
						require.Equal(t, want, finding.Attributes["url"])
					}
					for _, private := range []string{"fixture-password", "fixture-query", "fixture-fragment"} {
						require.NotContains(t, stdout.String(), private)
					}
				})
			}
		})
	}
}

func gitCmdT(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
}

func TestGitIncludeMerges(t *testing.T) {
	repo := t.TempDir()
	gitCmdT(t, repo, "init", "-q", "-b", "main")
	gitCmdT(t, repo, "config", "user.name", "Merge Test")
	gitCmdT(t, repo, "config", "user.email", "merge@example.test")
	write := func(name, value string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), []byte(value+"\n"), 0600))
		gitCmdT(t, repo, "add", name)
	}
	write("root.txt", "SECRET_ROOT")
	gitCmdT(t, repo, "commit", "-qm", "base")
	gitCmdT(t, repo, "checkout", "-qb", "side")
	write("side.txt", "SECRET_SIDE")
	gitCmdT(t, repo, "commit", "-qm", "side")
	sideSHA := gitCmdT(t, repo, "rev-parse", "HEAD")
	gitCmdT(t, repo, "rm", "side.txt")
	gitCmdT(t, repo, "commit", "-qm", "delete side secret")
	gitCmdT(t, repo, "checkout", "-q", "main")
	write("main.txt", "ordinary content")
	gitCmdT(t, repo, "commit", "-qm", "main")
	gitCmdT(t, repo, "merge", "--no-ff", "side", "-m", "SECRET_MERGE_MESSAGE")
	write("merge.txt", "SECRET_MERGE_CONTENT")
	gitCmdT(t, repo, "commit", "--amend", "--no-edit")
	mergeSHA := gitCmdT(t, repo, "rev-parse", "HEAD")
	gitCmdT(t, repo, "branch", "-D", "side")
	configPath := writeTestConfig(t, "[[rules]]\nid='merge-secret'\nregex='SECRET_[A-Z_]+'\n")
	for _, engine := range []string{"git", "go-git"} {
		for _, workers := range []int{1, 4} {
			for _, tc := range []struct {
				name      string
				flag      string
				wantMerge bool
			}{
				{"default", "", true},
				{"enabled", "--include-merges", true},
				{"explicit true", "--include-merges=true", true},
				{"disabled", "--include-merges=false", false},
				{"legacy alias", "--no-merge-patches", false},
			} {
				t.Run(fmt.Sprintf("%s/workers=%d/%s", engine, workers, tc.name), func(t *testing.T) {
					root, stdout := newTestCLI(t)
					args := []string{"git", repo, "--git-engine=" + engine, fmt.Sprintf("--git-workers=%d", workers), "--include=commit-messages", "--config", configPath, "--no-banner", "--exit-code=0", "--output=-"}
					if tc.flag != "" {
						args = append(args, tc.flag)
					}
					root.SetArgs(args)
					require.NoError(t, root.Execute())
					_, findings := decodeScanJSON(t, stdout.Bytes())
					var values []string
					for _, f := range findings {
						values = append(values, f.Match.Value)
						switch f.Match.Value {
						case "SECRET_SIDE":
							require.Equal(t, sideSHA, f.Attributes["git.sha"])
						case "SECRET_MERGE_MESSAGE", "SECRET_MERGE_CONTENT":
							require.Equal(t, mergeSHA, f.Attributes["git.sha"])
						}
					}
					want := []string{"SECRET_ROOT", "SECRET_SIDE", "SECRET_MERGE_MESSAGE"}
					if tc.wantMerge {
						want = append(want, "SECRET_MERGE_CONTENT")
					}
					require.ElementsMatch(t, want, values)
				})
			}
		}
	}
}

// runPreReceive drives the git command as a pre-receive hook with the given
// stdin, returning the exit code and captured stderr.
func runPreReceive(t *testing.T, repo, configPath, stdin string, extraArgs ...string) (int, string) {
	t.Helper()
	root, _ := newTestCLI(t)
	var stderr bytes.Buffer
	root.runtime.stderr = &stderr
	code := 0
	root.runtime.exit = func(n int) { code = n; panic(n) }
	root.SetIn(strings.NewReader(stdin))
	args := append([]string{
		"git", repo, "--pre-receive", "--no-banner", "--config", configPath,
	}, extraArgs...)
	root.SetArgs(args)
	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(int); !ok {
					panic(p)
				}
			}
		}()
		require.NoError(t, root.Execute())
	}()
	return code, stderr.String()
}

// TestPreReceiveHookRejectsPushWithLeak drives the full pre-receive flow: it
// feeds a ref update on stdin, confirms the command exits non-zero when the
// pushed commit contains a secret and prints the environment-expanded custom
// message, and confirms a delete-only push succeeds.
func TestPreReceiveHookRejectsPushWithLeak(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	gitCmdT(t, repo, "init", "--quiet")
	gitCmdT(t, repo, "config", "user.email", "test@example.com")
	gitCmdT(t, repo, "config", "user.name", "Test User")

	configPath := filepath.Join(t.TempDir(), "rules.toml")
	require.NoError(t, os.WriteFile(configPath,
		[]byte("[[rules]]\nid='hook-secret'\nregex='SECRET_PRIVATE'\n"), 0o600))

	// Clean base commit.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "clean.txt"), []byte("nothing here\n"), 0o600))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "--quiet", "-m", "base")
	oldSHA := gitCmdT(t, repo, "rev-parse", "HEAD")

	// Second commit introduces a detectable secret.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "creds.txt"), []byte("SECRET_PRIVATE\n"), 0o600))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "--quiet", "-m", "leak")
	newSHA := gitCmdT(t, repo, "rev-parse", "HEAD")

	t.Setenv("PROJECT", "my-service")

	// Update push (old..new) with a leak must be rejected with the message.
	code, stderr := runPreReceive(t, repo, configPath,
		oldSHA+" "+newSHA+" refs/heads/main\n",
		"--pre-receive-error-message", "BLOCKED on ${PROJECT}")
	require.Equal(t, 1, code, "push with a leak should exit non-zero")
	require.Contains(t, stderr, "BLOCKED on my-service", "custom message with expanded env var")
	require.Contains(t, stderr, "leaks found")

	// Delete-only push has nothing to scan and must succeed.
	zero := strings.Repeat("0", 40)
	code, stderr = runPreReceive(t, repo, configPath,
		newSHA+" "+zero+" refs/heads/main\n")
	require.Zero(t, code, "delete-only push should succeed")
	require.Contains(t, stderr, "no new commits to scan")
}

func TestPreReceiveHookRejectsMergeSecret(t *testing.T) {
	repo := t.TempDir()
	gitCmdT(t, repo, "init", "--quiet", "-b", "main")
	gitCmdT(t, repo, "config", "user.email", "test@example.com")
	gitCmdT(t, repo, "config", "user.name", "Test User")
	gitCmdT(t, repo, "commit", "--quiet", "--allow-empty", "-m", "base")
	oldSHA := gitCmdT(t, repo, "rev-parse", "HEAD")
	gitCmdT(t, repo, "checkout", "-qb", "side")
	gitCmdT(t, repo, "commit", "--quiet", "--allow-empty", "-m", "side")
	gitCmdT(t, repo, "checkout", "-q", "main")
	gitCmdT(t, repo, "merge", "--no-ff", "side", "-m", "merge")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "creds.txt"), []byte("SECRET_PRIVATE\n"), 0o600))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "--amend", "--no-edit")
	newSHA := gitCmdT(t, repo, "rev-parse", "HEAD")

	configPath := filepath.Join(t.TempDir(), "rules.toml")
	require.NoError(t, os.WriteFile(configPath,
		[]byte("[[rules]]\nid='hook-secret'\nregex='SECRET_PRIVATE'\n"), 0o600))
	code, stderr := runPreReceive(t, repo, configPath,
		oldSHA+" "+newSHA+" refs/heads/main\n")
	require.Equal(t, 1, code, "a secret introduced only in a merge must reject the push")
	require.Contains(t, stderr, "leaks found")
	code, _ = runPreReceive(t, repo, configPath,
		oldSHA+" "+newSHA+" refs/heads/main\n", "--include-merges=false")
	require.Zero(t, code, "explicitly suppressing merge patches must also apply to pre-receive scans")
}

func TestGoGitHistoryWithoutExecutable(t *testing.T) {
	repo := t.TempDir()
	gitCmdT(t, repo, "init", "-q", "-b", "main")
	gitCmdT(t, repo, "config", "user.email", "first@example.test")
	gitCmdT(t, repo, "config", "user.name", "First")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "root.txt"), []byte("before\nSECRET_ONE\nafter\n"), 0644))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "commit", "-qm", "root")
	rootSHA := gitCmdT(t, repo, "rev-parse", "HEAD")
	gitCmdT(t, repo, "rm", "-q", "root.txt")
	gitCmdT(t, repo, "commit", "-qm", "deleted")
	gitCmdT(t, repo, "checkout", "-qb", "side", rootSHA)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "branch.txt"), []byte("SECRET_TWO\n"), 0644))
	gitCmdT(t, repo, "add", ".")
	gitCmdT(t, repo, "-c", "user.name=Side", "commit", "-qm", "side")
	gitCmdT(t, repo, "checkout", "-q", "main")
	gitCmdT(t, repo, "repack", "-ad")
	configPath := writeTestConfig(t, "[[rules]]\nid='git-secret'\nregex='SECRET_[A-Z]+'\n")
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name  string
		args  []string
		count int
	}{
		{"all refs", nil, 2}, {"head", []string{"--revision=HEAD"}, 1}, {"author", []string{"--author=Side"}, 1}, {"date", []string{"--since=2030-01-01"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, stdout := newTestCLI(t)
			args := append([]string{"git", repo, "--git-engine=go-git", "--config", configPath, "--no-banner", "--exit-code=0", "--output=-"}, tc.args...)
			root.SetArgs(args)
			require.NoError(t, root.Execute())
			_, findings := decodeScanJSON(t, stdout.Bytes())
			require.Len(t, findings, tc.count)
			for _, finding := range findings {
				require.Len(t, finding.Attributes["git.blob_sha"], 40)
				require.Len(t, finding.Attributes["git.sha"], 40)
			}
		})
	}
}

func TestGoGitOptionValidation(t *testing.T) {
	for _, cmd := range []GitCmd{
		{GitEngine: "go-git", Staged: true}, {GitEngine: "go-git", LogOpts: "--since=2001-01-01"},
		{GitEngine: "go-git", Repo: "https://example.test/repo"}, {GitEngine: "go-git", Author: "["},
		{GitEngine: "go-git", Since: "yesterday"}, {GitEngine: "go-git", Until: "2001-01-01", Since: "2001-01-02"},
		{GitEngine: "go-git", All: true, Revision: "HEAD"}, {GitEngine: "git", Author: "Test"},
		{GitEngine: "go-git", GitCacheMiB: -1}, {GitEngine: "go-git", GitCacheMiB: 4097},
		{GitEngine: "git", GitCacheMiB: 32},
		{GitEngine: "git", GitDiskCacheMiB: -1},
		{GitEngine: "git", GitWorkers: -1}, {GitEngine: "git", GitWorkers: 17},
		{GitEngine: "go-git", GitDiskCacheMiB: -2}, {GitEngine: "go-git", GitDiskCacheMiB: 4097},
	} {
		require.Error(t, cmd.Validate())
	}
	require.NoError(t, (GitCmd{GitEngine: "go-git", Author: "Test", Since: "2001-01-01", GitWorkers: 4}).Validate())
	require.NoError(t, (GitCmd{GitEngine: "git", GitWorkers: 16}).Validate())
	source, err := (GitCmd{GitEngine: "go-git", GitCacheMiB: 32}).goGitSource()
	require.NoError(t, err)
	require.Equal(t, 32, source.CacheMiB)
	source, err = (GitCmd{GitEngine: "go-git", GitDiskCacheMiB: -1}).goGitSource()
	require.NoError(t, err)
	require.Equal(t, -1, source.DiskCacheMiB)
}
