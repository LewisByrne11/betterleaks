package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/betterleaks/betterleaks/v2/scan"
	"github.com/betterleaks/betterleaks/v2/sources/gogit"
	"github.com/betterleaks/betterleaks/v2/sources/scm"
)

type GitCmd struct {
	ScanFlags       `embed:""`
	MaxArchiveDepth int      `group:"scanning" name:"max-archive-depth" default:"8" help:"Allow scanning into nested archives up to this depth."`
	Token           string   `group:"source" help:"Token for an HTTP(S) clone (or the known host's GITHUB_TOKEN, GITLAB_TOKEN, HUGGINGFACE_TOKEN/HF_TOKEN)."`
	Platform        string   `group:"source" help:"Target platform used to generate links: github or gitlab."`
	Since           string   `group:"source" help:"Minimum committer date, RFC3339 or YYYY-MM-DD (UTC)."`
	Until           string   `group:"source" help:"Maximum committer date, RFC3339 or YYYY-MM-DD (UTC)."`
	Author          string   `group:"source" help:"Go regular expression matching author name and email."`
	Committer       string   `group:"source" help:"Go regular expression matching committer name and email."`
	Revision        string   `group:"source" help:"Starting revision; when omitted, scans all refs."`
	All             bool     `group:"source" help:"Scan history reachable from all refs (default unless --revision is set)."`
	MaxCount        int      `group:"source" name:"max-count" help:"Maximum selected commits; zero is unlimited."`
	GitWorkers      int      `group:"source" name:"git-workers" help:"Concurrent history readers (default up to 8; maximum 16)."`
	GitCacheMiB     int      `group:"source" name:"git-cache-mib" help:"Cached object content in MiB; zero uses 256 MiB. This is not a process memory limit."`
	GitDiskCacheMiB int      `group:"source" name:"git-disk-cache-mib" help:"Temporary object cache in MiB; zero uses 1024 MiB, -1 disables. Removed after scanning."`
	GitMaxBlobMiB   int      `group:"source" name:"git-max-blob-mib" default:"64" help:"Maximum diff input size; exceeding it reports an incomplete scan."`
	Exclude         []string `group:"source" help:"Remove Git resources: history, merges, loose-objects, commit-messages, notes, tag-messages, reflog-messages, reflog-history."`
	Include         []string `group:"source" help:"Add Git resources: history, merges, loose-objects, commit-messages, notes, tag-messages, reflog-messages, reflog-history (defaults: history with merges, loose-objects)."`
	Repo            string   `arg:"" optional:"" help:"Local repository or HTTP(S) repository URL to scan."`
}

func (cmd GitCmd) Validate() error {
	if err := cmd.ScanFlags.Validate(); err != nil {
		return err
	}
	if cmd.GitWorkers < 0 || cmd.GitWorkers > 16 {
		return errors.New("--git-workers must be between 0 and 16")
	}
	src, err := cmd.gitSource()
	if err != nil {
		return err
	}
	return src.Validate()
}

func (cmd *GitCmd) Run(cli *CLI, runtime *commandRuntime) error {
	runGit(runtime, &cli.GlobalFlags, cmd)
	return nil
}

func runGit(runtime *commandRuntime, globals *GlobalFlags, options *GitCmd) {
	// start timer
	start := time.Now()

	// grab source
	source := "."
	if options.Repo != "" {
		source = options.Repo
	}

	// setup config (aka, the thing that defines rules)
	ignoreSource := source
	remote := remoteGitURL(source)
	if remote {
		ignoreSource = ""
	}
	cfg := initConfig(runtime, globals, &options.ScanFlags)
	initDiagnostics(runtime, &options.ScanFlags)

	// create runner
	filters, err := loadScanFilters(runtime, cfg, options.IgnoreFile, ignoreSource)
	if err != nil {
		runtime.fatal("unable to prepare scan", "error", err)
		return
	}
	runner, err := newScanPipeline(runtime, globals, &options.ScanFlags, cfg, scan.WithIgnoredFingerprints(filters.fingerprints...))
	if err != nil {
		runtime.fatal("unable to prepare scan", "error", err)
		return
	}

	src, err := options.gitSource()
	if err != nil {
		runtime.fatal("invalid Git source", "error", err)
		return
	}
	src.Logger, src.Prefilter = runtime.Logger(), filters.shouldSkip

	findings := mustNewFindingCollector(runtime, &options.ScanFlags, globals.NoColor, start, cfg, "git", source)
	findings.startScan(runtime)
	summary, err := runner.Scan(runtime.Context, src, findings.Add)
	if err != nil {
		runtime.Logger().Error("failed to scan Git repository", "error", err)
	}

	findingSummaryAndExit(runtime, summary, runner.ValidationEnabled(), findings, options.ExitCode, start, err)
}

func (cmd GitCmd) gitSource() (*gogit.Source, error) {
	parseDate := func(value string) (*time.Time, error) {
		if value == "" {
			return nil, nil
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if date, err := time.Parse(layout, value); err == nil {
				return &date, nil
			}
		}
		return nil, fmt.Errorf("invalid history date %q; use RFC3339 or YYYY-MM-DD", value)
	}
	since, err := parseDate(cmd.Since)
	if err != nil {
		return nil, err
	}
	until, err := parseDate(cmd.Until)
	if err != nil {
		return nil, err
	}
	platform, err := scm.PlatformFromString(cmd.Platform)
	if err != nil {
		return nil, err
	}
	if cmd.GitMaxBlobMiB < 0 || cmd.GitMaxBlobMiB > 4096 {
		return nil, errors.New("--git-max-blob-mib must be between 0 and 4096")
	}
	if cmd.GitCacheMiB < 0 || cmd.GitCacheMiB > 4096 {
		return nil, errors.New("--git-cache-mib must be between 0 and 4096")
	}
	if cmd.GitDiskCacheMiB < -1 || cmd.GitDiskCacheMiB > 4096 {
		return nil, errors.New("--git-disk-cache-mib must be between -1 and 4096")
	}
	if cmd.All && cmd.Revision != "" {
		return nil, errors.New("--all and --revision are mutually exclusive")
	}
	repo := cmd.Repo
	if repo == "" {
		repo = "."
	}
	url, token := "", cmd.Token
	if remoteGitURL(repo) {
		url, repo = repo, ""
		if token == "" {
			token = remoteGitToken(url)
		}
	}
	return &gogit.Source{RepoPath: repo, URL: url, Token: token, Revision: cmd.Revision, All: cmd.All || cmd.Revision == "", Since: since, Until: until,
		Author: cmd.Author, Committer: cmd.Committer, MaxCount: cmd.MaxCount, Workers: cmd.GitWorkers, CacheMiB: cmd.GitCacheMiB, DiskCacheMiB: cmd.GitDiskCacheMiB,
		Prefilter: nil, Logger: nil, RemoteURL: "", MaxBlobBytes: int64(cmd.GitMaxBlobMiB) << 20, Include: cmd.Include, Exclude: cmd.Exclude, Platform: platform, MaxArchiveDepth: cmd.MaxArchiveDepth}, nil
}

// An existing path wins even when its spelling resembles a URL.
func remoteGitURL(target string) bool {
	if _, err := os.Stat(target); err == nil {
		return false
	}
	u, err := url.Parse(target)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func remoteGitToken(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	switch strings.ToLower(u.Host) {
	case "github.com":
		return os.Getenv("GITHUB_TOKEN")
	case "gitlab.com":
		return os.Getenv("GITLAB_TOKEN")
	case "huggingface.co":
		if token := os.Getenv("HUGGINGFACE_TOKEN"); token != "" {
			return token
		}
		return os.Getenv("HF_TOKEN")
	}
	return ""
}

// Match clone authentication, without forwarding environment tokens on redirects.
type gitAutoTransport struct {
	host  string
	token string
}

func (t gitAutoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" && req.URL.Host == t.host {
		req = req.Clone(req.Context())
		req.SetBasicAuth("x-access-token", t.token)
	}
	return http.DefaultTransport.RoundTrip(req)
}
