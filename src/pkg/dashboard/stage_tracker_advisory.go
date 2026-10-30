package dashboard

import (
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// An advisory issue (hivecommons/hive#15381): a `triage/advisory` issue, a
// dedupe checklist, or any body whose second line reads "Decision required:
// no" is a note for maintainers to read and close — no agent picks it up,
// nothing waits on it. The tracker says so instead of drawing it as a plain
// "Open · since 3h" row that reads as stalled work.

const (
	// StageTrackerAdvisoryKind is StageTrackerAdvisory.Kind; the row's own
	// kind stays "issue" (keys and URLs depend on it).
	StageTrackerAdvisoryKind = "advisory"
	// StageTrackerAdvisoryChip / StageTrackerAdvisoryEmptyChip are the Open
	// chevron's word (≤ 8 words). Empty is for a checklist that itself says
	// it found nothing actionable.
	StageTrackerAdvisoryChip      = "Advisory · read & close"
	StageTrackerAdvisoryEmptyChip = "Advisory · nothing found · safe to close"
	// StageTrackerAdvisoryCaption / StageTrackerAdvisoryEmptyCaption are
	// the row captions.
	StageTrackerAdvisoryCaption      = "Advisory for maintainers · read, then close"
	StageTrackerAdvisoryEmptyCaption = "Advisory for maintainers · nothing found · safe to close"
	// stageTrackerAdvisoryWhy is the StageActor sentence.
	stageTrackerAdvisoryWhy = "advisory for maintainers — read it, then close it; nothing waits on it and no agent picks it up"

	stageTrackerAdvisorySourceLabel     = "label"
	stageTrackerAdvisorySourceChecklist = "dedupe-checklist"
	stageTrackerAdvisorySourceVerdict   = "decision-no"
)

// StageTrackerAdvisory is the payload block on an advisory row.
type StageTrackerAdvisory struct {
	Kind string `json:"kind"`
	// Source says what classified it: label (triage/advisory),
	// dedupe-checklist (the body marker) or decision-no (the verdict line).
	Source string `json:"source"`
	// Empty is true when the advisory's own body says it found nothing.
	Empty   bool   `json:"empty,omitempty"`
	Chip    string `json:"chip"`
	Caption string `json:"caption"`
}

// stageTrackerAdvisoryFor classifies an issue from its labels, its body
// (when the caller has it) and the enumeration's body-derived hint (when
// it does not). nil for an ordinary issue.
func stageTrackerAdvisoryFor(labels []string, body string, hint *github.IssueAdvisory) *StageTrackerAdvisory {
	if hint == nil && strings.TrimSpace(body) != "" {
		hint = github.IssueAdvisoryFromBody(body)
	}
	adv := &StageTrackerAdvisory{Kind: StageTrackerAdvisoryKind}
	switch {
	case slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), github.DedupeAdvisoryLabelName) }):
		adv.Source = stageTrackerAdvisorySourceLabel
	case hint != nil && hint.DedupeChecklist:
		adv.Source = stageTrackerAdvisorySourceChecklist
	case hint != nil && hint.DecisionNo:
		adv.Source = stageTrackerAdvisorySourceVerdict
	default:
		return nil
	}
	adv.Empty = hint != nil && hint.Empty
	adv.Chip, adv.Caption = StageTrackerAdvisoryChip, StageTrackerAdvisoryCaption
	if adv.Empty {
		adv.Chip, adv.Caption = StageTrackerAdvisoryEmptyChip, StageTrackerAdvisoryEmptyCaption
	}
	return adv
}

// stageTrackerAdvisoryMods are the pills an advisory never wears: it is
// not work, so it cannot stall or be sent back.
var stageTrackerAdvisoryMods = []string{modStalled, modRegressed}

// applyStageTrackerAdvisory marks it as an advisory: the caption and the
// actor say "read, then close", the stall/regression pills come off, the
// 000-new block (which would say "waiting for triage") is dropped, and the
// stage actor is a human with nothing blocking on them.
func applyStageTrackerAdvisory(it *StageTrackerItem, adv *StageTrackerAdvisory) {
	if it == nil || adv == nil {
		return
	}
	it.Advisory = adv
	it.Caption = adv.Caption
	it.Actor = "maintainer"
	it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerAdvisoryMods, m) })
	if it.New != nil {
		it.New = nil
		if it.StageSlug == StageTrackerNewSlug {
			it.StageSlug = ""
		}
	}
	it.StageActor = &StageTrackerActor{Kind: StageActorHuman, Why: stageTrackerAdvisoryWhy, Since: it.StageSince}
}
