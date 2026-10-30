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

// githubLabelDescriptionMax is GitHub's cap on a label description. A
// longer value makes POST/PATCH /labels answer 422 "description is too
// long", which used to drop the label from every CreateIssue.
const githubLabelDescriptionMax = config.GitHubLabelDescriptionMax

// clampLabelDescription trims desc and shortens it to fit GitHub's label
// description cap, ending an over-long value with an ellipsis. It counts
// runes so a multibyte description is never cut mid-character.
func clampLabelDescription(desc string) string {
	desc = strings.TrimSpace(desc)
	runes := []rune(desc)
	if len(runes) <= githubLabelDescriptionMax {
		return desc
	}
	return strings.TrimSpace(string(runes[:githubLabelDescriptionMax-1])) + "…"
}

// NeedsDecisionStageLabel is the single "issue parked for a human ruling"
// signal (hivecommons/hive#12683): `stage/045-needs-decision`, the issue-side
// parking stage. On an issue it is an INPUT as well as the stage — an agent,
// the relay or a person puts it on; the enumeration hard-suppresses the
// issue; the stage reconciler keeps it until a maintainer un-parks with
// `/hive approve` / `/hive decision` or removes the label.
const NeedsDecisionStageLabel = config.DefaultNeedsDecisionLabel

// legacyIssueNeedsDecisionStageLabel is `stage/085-needs-decision` as found
// on an ISSUE: the pre-045 park spelling (owner decision: 085 is PR-only).
// Readers treat it exactly like NeedsDecisionStageLabel until the stage
// reconciler has swapped it; its repo definition is never deleted because
// PRs still wear it.
const legacyIssueNeedsDecisionStageLabel = config.PRNeedsDecisionStageLabel

// PRNeedsDecisionStageLabel is `stage/085-needs-decision`, the PR parking
// stage (and, on an issue, the retired pre-045 spelling).
const PRNeedsDecisionStageLabel = legacyIssueNeedsDecisionStageLabel

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
	legacyIssueNeedsDecisionStageLabel,
	issueNeedsSpecLabel,
	issueNeedsDecisionLabel,
	issueNeedsDirectionLabel,
	PossiblyExistsStageLabel,
}

// PossiblyExistsStageLabel is the prior-art gate's park: the issue stage
// label stage/017-possible-existing itself, as 045 is the decision park
// (hivecommons/hive#13575). While an issue wears it no agent claims it; the
// author's or a maintainer's reply lifts it (SweepPriorArtGateResume).
const PossiblyExistsStageLabel = config.PossiblyExistsStageLabel

// AlreadyExistsLabel marks an issue closed by `/hive decision exists`: the
// request was already available and the gate comment says where.
const AlreadyExistsLabel = "already-exists"

// legacyDecisionLabels are the retired park labels the stage label replaced
// and whose repo DEFINITIONS are deleted once nothing carries them. The
// pre-045 `stage/085-needs-decision` is not among them: on an issue it is
// retired too, but the definition stays for PRs.
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
	AlreadyExistsLabel: {
		color:       "cfd3d7",
		description: "Closed by the prior-art gate: the request was already available (see the findings comment)",
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
