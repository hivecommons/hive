package config

// ForgeSweepSupport says which hive-driven sweeps exist for a forge kind.
// The GitHub sweeps (pkg/github/automerge and the close-on-merge ledger) call
// the GitHub API directly and run only for repos RepoOnGitHub admits. Another
// forge gets these sweeps only through its own implementation, listed here.
type ForgeSweepSupport struct {
	// AutoMerge is true when the forge has an automerge sweep.
	AutoMerge bool
	// CloseOnMerge is true when the forge has a close-on-merge sweep.
	CloseOnMerge bool
}

// ForgeSweepsFor returns the sweeps implemented for kind.
//
//   - github: both, through the GitHub sweeps.
//   - forgejo: both, through pkg/forgesweep on Forgejo's REST API.
//   - gitea and gitlab: NOT implemented. Both sweeps stay off for repos on
//     these forges. Gitea shares Forgejo's REST client, but its API has
//     diverged (#19493) and these sweeps were never run against it, so it is
//     deliberately not enabled by sharing the Forgejo code.
//
// An unknown kind is treated as GitHub, the same fallback RepoOnGitHub takes.
func ForgeSweepsFor(kind string) ForgeSweepSupport {
	switch kind {
	case ForgeForgejo:
		return ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}
	case ForgeGitea, ForgeGitLab:
		return ForgeSweepSupport{}
	}
	return ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}
}

// ForgejoSweepRepos returns the active (not paused) repos that resolve to
// Forgejo, the repos the Forgejo sweeps act on.
func (c *Config) ForgejoSweepRepos() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, repo := range c.ActiveRepos() {
		if c.ForgeKindForRepo(repo) == ForgeForgejo {
			out = append(out, repo)
		}
	}
	return out
}
