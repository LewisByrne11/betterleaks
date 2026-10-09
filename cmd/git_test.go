package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitCmdT(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
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
	} {
		require.Error(t, cmd.Validate())
	}
	require.NoError(t, (GitCmd{GitEngine: "go-git", Author: "Test", Since: "2001-01-01", GitWorkers: 4}).Validate())
}
