package scheduler

import (
	"context"
	"time"

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
// reasoning instead of rebuilding it from the diff. Appended to the dynamic
// tail directly after the fix-before-new blocks (kick_layout.go), next to the
// lists it explains.
func (s *Scheduler) addPRFollowUpHandoff(agentName string, message string) string {
	if message == "" || s.cfg == nil || !s.cfg.PRFollowUpResumeEnabled() || !s.isPRCapableAgent(agentName) {
		return message
	}
	// The section also offers the backend-native resume handle captured when
	// the PR opened (#9606), unless it is older than the configured
	// staleness limit.
	section := prfollowup.HandoffSectionWithResume(context.Background(), prFollowUpDir(),
		s.cfg.PRFollowUpResumeIDMaxAge(), time.Now(), agentName, s.cfg.BaseAgentName(agentName))
	if section == "" {
		return message
	}
	return appendKickSection(message, section)
}
