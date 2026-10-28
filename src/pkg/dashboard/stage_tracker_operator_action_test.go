package dashboard

import (
	"strings"
	"testing"
)

// stageTrackerOperatorJS supplies the decision helpers used by both row
// rendering and filtering. Keep the fixtures on the shipped implementation.
func stageTrackerOperatorJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_OPERATOR_STUCK_MS"),
		jsFunc(t, html, "stageTrackerOperatorStuck"),
		jsFunc(t, html, "stageTrackerDecisionMatches"),
		jsFunc(t, html, "stageTrackerOperatorStage"),
		jsFunc(t, html, "stageTrackerOperatorTooltip"),
		jsFunc(t, html, "stageTrackerOperatorBadgeHtml"),
		jsFunc(t, html, "stageTrackerOperatorPRBadge"),
	}, "\n")
}
