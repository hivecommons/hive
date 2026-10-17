package github

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type issueLabelDefinition struct {
	color       string
	description string
}

// NeedsDecisionStageLabel is the single "parked for a human ruling" signal
// (hivecommons/hive#12683): `stage/085-needs-decision`, the same stage label
// the PR lanes compute. On an issue it is an INPUT as well as the stage —
// an agent, the relay or a person puts it on; the enumeration hard-suppresses
// the issue; the stage reconciler keeps it until a maintainer un-parks with
// `/hive approve` / `/hive decision` or removes the label.
const NeedsDecisionStageLabel = config.DefaultNeedsDecisionLabel

const (
	// issueNeedsDirectionLabel and issueNeedsDecisionLabel are RETIRED: both
	// parked an issue exactly like the stage label and are migrated to it by
	// the issue stage reconciler (legacy_decision_labels.go). Readers keep
	// recognising them so an issue never un-parks mid-migration.
	issueNeedsDirectionLabel            = config.LegacyNeedsDirectionLabel
	issueNeedsDecisionLabel             = config.LegacyNeedsDecisionLabel
	issueNeedsSpecLabel                 = "needs-spec"
	issueNeedsReporterConfirmationLabel = "needs-reporter-confirmation"
)

// NeedsSignalLabel is the escalation label hive applies when it needs a
// better CI or test signal before continuing. It is not a hard-suppress
// label (the issue stays actionable), but it is still "waiting on a person",
// so the dashboard's Needs-human preset includes it.
const NeedsSignalLabel = "needs-signal"

// hardSuppressIssueLabels are the escalation labels that park an issue on a
// person ("Hard suppress until directed" in docs/labels-and-control-signals.md).
// An issue carrying one never enters the actionable set, so no agent kick or
// contributor offer can name or claim it. Hold and do-not-merge labels are
// handled by the hold list and exempt filter respectively.
var hardSuppressIssueLabels = []string{
	issueNeedsHumanLabel,
	NeedsDecisionStageLabel,
	issueNeedsSpecLabel,
	issueNeedsDecisionLabel,
	issueNeedsDirectionLabel,
}

// legacyDecisionLabels are the retired park labels the stage label replaced.
var legacyDecisionLabels = config.LegacyNeedsDecisionLabels

func isLegacyDecisionLabel(label string) bool {
	for _, legacy := range legacyDecisionLabels {
		if strings.EqualFold(strings.TrimSpace(label), legacy) {
			return true
		}
	}
	return false
}

// hasLegacyDecisionLabel reports whether labels carry a retired park label.
func hasLegacyDecisionLabel(labels []string) bool {
	for _, l := range labels {
		if isLegacyDecisionLabel(l) {
			return true
		}
	}
	return false
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
	issueNeedsReporterConfirmationLabel: {
		color:       "fbca04",
		description: "Hive is waiting for the issue reporter or a maintainer to confirm the fix",
	},
	issueNeedsSpecLabel: {
		color:       "bfd4f2",
		description: "Hive needs a specification or acceptance criteria before continuing",
	},
	NeedsSignalLabel: {
		color:       "fbca04",
		description: "Hive needs a better CI or test signal before continuing",
	},
	PartiallyAddressedLabel: {
		color:       PartiallyAddressedLabelColor,
		description: PartiallyAddressedLabelDescription,
	},
	LikelyDoneLabel: {
		color:       LikelyDoneLabelColor,
		description: LikelyDoneLabelDescription,
	},
	"meta": {
		color:       "5319e7",
		description: "Tracker for multiple open items sharing one root cause",
	},
}

func escalationIssueLabelDefinition(name string) (issueLabelDefinition, bool) {
	if def, ok := escalationIssueLabelDefinitions[name]; ok {
		return def, true
	}
	// The stage family (stage/085-needs-decision in particular) is created
	// with its own colour when a relay or agent asks for it by name.
	for _, def := range stagelabels.AllLabelDefs() {
		if strings.EqualFold(def.Name, name) {
			return issueLabelDefinition{color: def.Color, description: def.Description}, true
		}
	}
	return issueLabelDefinition{}, false
}
