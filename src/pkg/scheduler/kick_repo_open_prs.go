package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// repoOpenPRsKickNotes is the kick's line for each listed repo whose open-PRs
// mode (project.repo_policies[].open_prs, or project.open_prs: false) limits
// pull requests. Empty when every repo is on. The hive's PR relays enforce
// the mode; this tells the agent before it spends a session on a branch.
func repoOpenPRsKickNotes(cfg *config.Config, org string, repos []string) string {
	if cfg == nil {
		return ""
	}
	var b strings.Builder
	for _, repo := range repos {
		full := config.QualifyRepo(org, repo)
		switch cfg.RepoOpenPRsMode(full) {
		case config.RepoOpenPRsOff:
			fmt.Fprintf(&b, "🚫 PRs are off for %s: do not open pull requests there (hive-open-pr refuses them). Analyse, comment, and file findings as issues instead.\n", full)
		case config.RepoOpenPRsWorkflowOnly:
			fmt.Fprintf(&b, "🔒 %s accepts only workflow hand-off PRs: open a pull request there only for a workflow hand-off the hive gave you (a kick headed \"Workflow hand-off\"). For your own cadence work (coverage gaps, test hardening and the like), file findings as issues instead; hive-open-pr refuses those PRs.\n", full)
		}
	}
	return b.String()
}
