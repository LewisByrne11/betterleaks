package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/betterleaks/betterleaks/v2/pipeline"
	"github.com/betterleaks/betterleaks/v2/scan"
	"github.com/betterleaks/betterleaks/v2/sources"
	"github.com/betterleaks/betterleaks/v2/sources/gogit"
	"github.com/betterleaks/betterleaks/v2/sources/scm"
)

// multipleErrors wraps multiple scan errors into a single error that supports
// errors.Unwrap so callers can inspect individual errors.
type multipleErrors struct {
	msg  string
	errs []error
}

func (e *multipleErrors) Error() string   { return e.msg }
func (e *multipleErrors) Unwrap() []error { return e.errs }

type GitCmd struct {
	ScanFlags       `embed:""`
	MaxArchiveDepth int      `group:"scanning" name:"max-archive-depth" default:"8" help:"Allow scanning into nested archives up to this depth."`
	Token           string   `group:"source" help:"Token for an HTTP(S) clone (or the known host's GITHUB_TOKEN, GITLAB_TOKEN, HUGGINGFACE_TOKEN/HF_TOKEN)."`
	Platform        string   `group:"source" help:"Target platform used to generate links: github or gitlab."`
	Staged          bool     `group:"source" help:"Scan added lines in staged changes."`
	Unstaged        bool     `group:"source" help:"Scan added lines in unstaged changes to tracked files."`
	PreReceive      bool     `group:"source" name:"pre-receive" help:"Run as a Git pre-receive hook, scanning pushed commits read from stdin."`
	PreReceiveError string   `group:"source" name:"pre-receive-error-message" help:"Message printed to stderr when the pre-receive hook finds leaks; environment variables in $$VAR and $${VAR} form are expanded."`
	LogOpts         string   `group:"source" name:"log-opts" help:"Git log options (uses one history stream to preserve option semantics)."`
	GitEngine       string   `group:"source" name:"git-engine" default:"git" enum:"git,go-git" help:"History reader: git subprocesses or the experimental local go-git object reader."`
	Since           string   `group:"source" help:"go-git: minimum committer date, RFC3339 or YYYY-MM-DD (UTC)."`
	Until           string   `group:"source" help:"go-git: maximum committer date, RFC3339 or YYYY-MM-DD (UTC)."`
	Author          string   `group:"source" help:"go-git: Go regular expression matching author name and email."`
	Committer       string   `group:"source" help:"go-git: Go regular expression matching committer name and email."`
	Revision        string   `group:"source" help:"go-git: starting revision; when omitted, scans all refs."`
	All             bool     `group:"source" help:"go-git: scan history reachable from all refs (default unless --revision is set)."`
	NoMergePatches  bool     `group:"source" name:"no-merge-patches" help:"go-git: suppress merge patches, matching plain git log -p. By default merges are compared with their first parent."`
	MaxCount        int      `group:"source" name:"max-count" help:"go-git: maximum selected commits; zero is unlimited."`
	GitWorkers      int      `group:"source" name:"git-workers" help:"go-git: concurrent history workers (default up to 4, maximum 16)."`
	GitMaxBlobMiB   int      `group:"source" name:"git-max-blob-mib" default:"64" help:"go-git: maximum diff input size; exceeding it reports an incomplete scan."`
	Include         []string `group:"source" help:"Additional Git resources to scan: commit-messages, tag-messages, reflogs."`
	Repo            string   `arg:"" optional:"" help:"Local repository or HTTP(S) repository URL to scan."`
}

func (cmd GitCmd) Validate() error {
	if err := cmd.ScanFlags.Validate(); err != nil {
		return err
	}
	if cmd.GitEngine != "" && cmd.GitEngine != "git" && cmd.GitEngine != "go-git" {
		return fmt.Errorf("unknown Git engine %q", cmd.GitEngine)
	}
	if cmd.GitEngine == "go-git" {
		if cmd.Staged || cmd.Unstaged || cmd.PreReceive || remoteGitURL(cmd.Repo) || cmd.Token != "" {
			return errors.New("go-git currently supports local committed history only; staged, unstaged, pre-receive and remote scans are unsupported")
		}
		if cmd.LogOpts != "" {
			return errors.New("go-git does not accept arbitrary --log-opts; use --since, --until, --author, --committer, --revision, --all or --max-count")
		}
		src, err := cmd.goGitSource()
		if err != nil {
			return err
		}
		return src.Validate()
	}
	if cmd.Since != "" || cmd.Until != "" || cmd.Author != "" || cmd.Committer != "" || cmd.Revision != "" || cmd.All || cmd.NoMergePatches || cmd.MaxCount != 0 || cmd.GitWorkers != 0 || (cmd.GitMaxBlobMiB != 0 && cmd.GitMaxBlobMiB != 64) {
		return errors.New("structured history filters and worker/blob settings require --git-engine=go-git")
	}
	if cmd.Staged && cmd.Unstaged {
		return errors.New("--staged and --unstaged are mutually exclusive")
	}
	if cmd.PreReceive && (cmd.Staged || cmd.Unstaged) {
		return errors.New("--pre-receive cannot be combined with --staged or --unstaged")
	}
	if cmd.PreReceive && cmd.LogOpts != "" {
		return errors.New("--pre-receive cannot be combined with --log-opts")
	}
	if cmd.PreReceive && remoteGitURL(cmd.Repo) {
		return errors.New("--pre-receive requires a local Git repository")
	}
	if cmd.PreReceive && len(cmd.Include) > 0 {
		return errors.New("--include cannot be combined with --pre-receive")
	}
	if remoteGitURL(cmd.Repo) && (cmd.Staged || cmd.Unstaged) {
		return errors.New("--staged and --unstaged require a local Git repository")
	}
	if len(cmd.Include) > 0 && (cmd.Staged || cmd.Unstaged) {
		return errors.New("--include requires a Git history scan; it cannot be combined with --staged or --unstaged")
	}
	return (&sources.Git{Include: cmd.Include}).Validate()
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
		if source == "" {
			source = "."
		}
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

	var src sources.Source

	if options.GitEngine == "go-git" {
		goSource, sourceErr := options.goGitSource()
		if sourceErr != nil {
			runtime.fatal("invalid go-git source", "error", sourceErr)
			return
		}
		goSource.Logger, goSource.Prefilter = runtime.Logger(), filters.shouldSkip
		src = goSource
	} else if options.PreReceive {
		updates, parseErr := sources.ParsePreReceiveInput(runtime.stdin)
		if parseErr != nil {
			runtime.fatal("could not read pre-receive input", "error", parseErr)
			return
		}
		logArgs := sources.PreReceiveLogArgs(updates, sources.NewGitCommitResolver(runtime.Context, source))
		if len(logArgs) == 0 {
			// Nothing to scan (e.g. only ref deletions). Report cleanly.
			runtime.Logger().Info("pre-receive: no new commits to scan")
			findings := mustNewFindingCollector(runtime, &options.ScanFlags, globals.NoColor, start, cfg, "git", source)
			findings.startScan(runtime)
			findingSummaryAndExit(runtime, pipeline.ScanSummary{}, runner.ValidationEnabled(), findings, options.ExitCode, start, nil)
			return
		}
		// Server-side hook scans have no remote to link findings to.
		src = &sources.Git{
			Logger:          runtime.Logger(),
			RepoPath:        source,
			Prefilter:       filters.shouldSkip,
			Platform:        scm.NoPlatform,
			MaxArchiveDepth: options.MaxArchiveDepth,
			LogOpts:         strings.Join(logArgs, " "),
		}
	} else if options.Unstaged || options.Staged {
		mode := sources.GitWorkingTree
		if options.Staged {
			mode = sources.GitStaged
		}
		// Local diffs have no committed revision to link to.
		src = &sources.Git{
			Logger:          runtime.Logger(),
			RepoPath:        source,
			Mode:            mode,
			Prefilter:       filters.shouldSkip,
			Platform:        scm.NoPlatform,
			MaxArchiveDepth: options.MaxArchiveDepth,
		}
	} else {
		scmPlatform, platformErr := scm.PlatformFromString(options.Platform)
		if platformErr != nil {
			runtime.fatal("invalid platform", "error", platformErr)
		}
		resolvedPlatform, remoteURL := scmPlatform, ""
		if !remote {
			resolvedPlatform, remoteURL = sources.ResolveRemote(runtime.Context, scmPlatform, source)
		}

		gitSource := &sources.Git{
			Logger:          runtime.Logger(),
			RepoPath:        source,
			Prefilter:       filters.shouldSkip,
			Platform:        resolvedPlatform,
			RemoteURL:       remoteURL,
			MaxArchiveDepth: options.MaxArchiveDepth,
			LogOpts:         options.LogOpts,
			Include:         options.Include,
		}
		if remote {
			gitSource.RepoPath = ""
			gitSource.URL = source
			gitSource.Token = options.Token
			if gitSource.Token == "" {
				gitSource.Token = remoteGitToken(source)
			}
		}
		src = gitSource
	}

	findings := mustNewFindingCollector(runtime, &options.ScanFlags, globals.NoColor, start, cfg, "git", source)
	findings.startScan(runtime)
	summary, err := runner.Scan(runtime.Context, src, findings.Add)
	if err != nil {
		runtime.Logger().Error("failed to scan Git repository", "error", err)
	}

	// When running as a pre-receive hook, print the custom error message
	// (with environment variables expanded) so the pushing client sees it.
	if options.PreReceive && options.PreReceiveError != "" && findings.Count() != 0 {
		fmt.Fprintln(runtime.stderr, os.ExpandEnv(options.PreReceiveError))
	}

	findingSummaryAndExit(runtime, summary, runner.ValidationEnabled(), findings, options.ExitCode, start, err)
}

func (cmd GitCmd) goGitSource() (*gogit.Source, error) {
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
	if cmd.All && cmd.Revision != "" {
		return nil, errors.New("--all and --revision are mutually exclusive")
	}
	repo := cmd.Repo
	if repo == "" {
		repo = "."
	}
	return &gogit.Source{RepoPath: repo, Revision: cmd.Revision, All: cmd.All || cmd.Revision == "", NoMergePatches: cmd.NoMergePatches, Since: since, Until: until,
		Author: cmd.Author, Committer: cmd.Committer, MaxCount: cmd.MaxCount, Workers: cmd.GitWorkers,
		MaxBlobBytes: int64(cmd.GitMaxBlobMiB) << 20, Include: cmd.Include, Platform: platform, MaxArchiveDepth: cmd.MaxArchiveDepth}, nil
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
