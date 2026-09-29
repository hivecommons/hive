package scheduler

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/prfollowup"
)

// prFollowUpDir resolves the PR follow-up pointer directory; a var so tests
// can point the kick builder at a fixture store.
var prFollowUpDir = prfollowup.Dir

// addPRFollowUpHandoff prepends the PR handoff section (hivecommons/hive#9583)
// for PR-capable agents while turn.pr_follow_up is on: for each of the
// agent's own PRs that still has follow-up activity, the compact note of why
// and how the PR was built, plus any human feedback that could not be
// resumed into the authoring session. Regular kicks still /clear the
// conversation; this is what lets the fresh session start from the original
// reasoning instead of rebuilding it from the diff. Inserted at the same
// below-the-header seam as the fix-before-new blocks, after them, so it lands
// on top: the note is read before the lists it explains.
func (s *Scheduler) addPRFollowUpHandoff(agentName string, message string) string {
	if message == "" || s.cfg == nil || !s.cfg.PRFollowUpResumeEnabled() || !s.isPRCapableAgent(agentName) {
		return message
	}
	section := prfollowup.HandoffSection(context.Background(), prFollowUpDir(), agentName, s.cfg.BaseAgentName(agentName))
	if section == "" {
		return message
	}
	if idx := strings.Index(message, "\n"); idx >= 0 && strings.HasPrefix(message, "[agent:") {
		return message[:idx+1] + section + message[idx+1:]
	}
	return section + message
}
