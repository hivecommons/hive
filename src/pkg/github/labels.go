package github

import "strings"

type issueLabelDefinition struct {
	color       string
	description string
}

const (
	issueNeedsDirectionLabel            = "needs-direction"
	issueNeedsSpecLabel                 = "needs-spec"
	issueNeedsReporterConfirmationLabel = "needs-reporter-confirmation"
)

// hardSuppressIssueLabels are the escalation labels that park an issue on a
// person ("Hard suppress until directed" in docs/labels-and-control-signals.md).
// An issue carrying one never enters the actionable set, so no agent kick or
// contributor offer can name or claim it. Hold and do-not-merge labels are
// handled by the hold list and exempt filter respectively.
var hardSuppressIssueLabels = []string{
	issueNeedsHumanLabel,
	issueNeedsDirectionLabel,
	issueNeedsDecisionLabel,
	issueNeedsSpecLabel,
}

func hasHardSuppressIssueLabel(labels []string) bool {
	return hardSuppressIssueLabel(labels) != ""
}

func hardSuppressIssueLabel(labels []string) string {
	for _, label := range labels {
		label = strings.TrimSpace(label)
		for _, suppress := range hardSuppressIssueLabels {
			if strings.EqualFold(label, suppress) {
				return suppress
			}
		}
	}
	return ""
}

var escalationIssueLabelDefinitions = map[string]issueLabelDefinition{
	issueNeedsDirectionLabel: {
		color:       "d4c5f9",
		description: "Hive needs a maintainer direction decision before continuing",
	},
	issueNeedsReporterConfirmationLabel: {
		color:       "fbca04",
		description: "Hive is waiting for the issue reporter or a maintainer to confirm the fix",
	},
	issueNeedsSpecLabel: {
		color:       "bfd4f2",
		description: "Hive needs a specification or acceptance criteria before continuing",
	},
	"needs-signal": {
		color:       "fbca04",
		description: "Hive needs a better CI or test signal before continuing",
	},
	"meta": {
		color:       "5319e7",
		description: "Tracker for multiple open items sharing one root cause",
	},
}

func escalationIssueLabelDefinition(name string) (issueLabelDefinition, bool) {
	def, ok := escalationIssueLabelDefinitions[name]
	return def, ok
}
