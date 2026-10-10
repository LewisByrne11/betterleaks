package gogit

import (
	"github.com/betterleaks/betterleaks/v2/sources/scm"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// resolveRemote reads only local configuration. Match Git's default fetch
// remote: the current branch's remote, otherwise the sole remote or origin.
// Link metadata is optional and must not prevent a history scan.
func (s *Source) resolveRemote(r *git.Repository) (scm.Platform, string) {
	if s.Platform == scm.NoPlatform || s.RemoteURL != "" {
		return scm.ResolveRemoteURL(s.Platform, s.RemoteURL)
	}
	cfg, err := r.Config()
	if err != nil {
		return s.Platform, ""
	}
	name := "origin"
	if len(cfg.Remotes) == 1 {
		for n := range cfg.Remotes {
			name = n
		}
	}
	if head, err := r.Storer.Reference(plumbing.HEAD); err == nil && head.Type() == plumbing.SymbolicReference && head.Target().IsBranch() {
		if branch := cfg.Branches[head.Target().Short()]; branch != nil && branch.Remote != "" {
			name = branch.Remote
		}
	}
	remote := cfg.Remotes[name]
	if remote == nil || len(remote.URLs) == 0 {
		return s.Platform, ""
	}
	return scm.ResolveRemoteURL(s.Platform, remote.URLs[0])
}
