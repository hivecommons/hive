package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// labelDrivenRefusesUnparkCommands reports whether the un-park sweep answers
// `/hive approve` and `/hive decision` on repo with the label-driven pointer
// instead of acting: the repo opted in to label-driven triage and label
// approval is off for it (an empty approval-label list), which is exactly
// when the sweep's resolver reports no policy.
func labelDrivenRefusesUnparkCommands(cfg *config.Config, repo string) bool {
	return cfg != nil && cfg.RepoLabelDriven(repo) && len(cfg.LabelDrivenApprovalLabels(repo)) == 0
}

// markLabelDrivenReplies flags every your-turn issue decision on a repo whose
// sweep refuses the `/hive` un-park commands, so the "your turn" menu offers
// the triage-label guidance instead of commands that would be refused.
func markLabelDrivenReplies(decisions map[string]Ownership, repo string, env ownershipEnv) {
	if env.unparkCommandsRefused == nil || !env.unparkCommandsRefused(repo) {
		return
	}
	prefix := ownershipKindIssue + "#"
	for key, decision := range decisions {
		if !decision.YourTurn || !strings.HasPrefix(key, prefix) {
			continue
		}
		decision.LabelDriven = true
		decisions[key] = decision
	}
}
