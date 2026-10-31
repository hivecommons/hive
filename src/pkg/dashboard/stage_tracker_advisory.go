package dashboard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An advisory issue (hivecommons/hive#15381): a `triage/advisory` issue, a
// dedupe checklist, or any body whose second line reads "Decision required:
// no" is a note for maintainers to read and close — no agent picks it up,
// nothing waits on it. The tracker says so instead of drawing it as a plain
// "Open · since 3h" row that reads as stalled work. An advisory with a
// checklist is worked by the hive itself (hivecommons/hive#15754): the
// close-on-merge sweep ticks items as they close and closes the advisory
// once all are resolved, so the tracker never parks it on a person — no
// 045 stage, no needs-decision block, no blocker; the only lead is an
// optional "Close now".

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
	// StageTrackerAdvisoryResolvedChip / Caption are for a checklist whose
	// every item is resolved; the sweep closes it on its next pass.
	StageTrackerAdvisoryResolvedChip    = "Advisory · all items resolved · closing"
	StageTrackerAdvisoryResolvedCaption = "Advisory · all items resolved · the hive closes it on its next sweep"
	// stageTrackerAdvisoryWhy is the StageActor sentence.
	stageTrackerAdvisoryWhy = "advisory for maintainers — read it, then close it; nothing waits on it and no agent picks it up"
	// stageTrackerAdvisoryWorkingWhy is the StageActor sentence for a
	// checklist the hive is still working.
	stageTrackerAdvisoryWorkingWhy = "advisory checklist — the hive ticks each item as it closes or merges and closes the advisory when all are resolved; nothing waits on you"

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
	Empty bool `json:"empty,omitempty"`
	// Resolved / Total count the checklist's task lines (Total 0: no
	// checklist). The hive works them; the row is never parked on a person.
	Resolved int    `json:"resolved,omitempty"`
	Total    int    `json:"total,omitempty"`
	Chip     string `json:"chip"`
	Caption  string `json:"caption"`
}

// StageTrackerAdvisoryWorkingChip / Caption are the chip and caption of a
// checklist the hive is still working: "Advisory · hive is working the
// items (2/5 resolved)".
func StageTrackerAdvisoryWorkingChip(resolved, total int) string {
	return fmt.Sprintf("Advisory · hive is working the items (%d/%d resolved)", resolved, total)
}

func StageTrackerAdvisoryWorkingCaption(resolved, total int) string {
	return fmt.Sprintf("Advisory · the hive is working the items (%d/%d resolved) · closes itself when done", resolved, total)
}

// Working reports a checklist with items still open.
func (a *StageTrackerAdvisory) Working() bool {
	return a != nil && a.Total > 0 && a.Resolved < a.Total
}

// AllResolved reports a checklist whose every item is resolved.
func (a *StageTrackerAdvisory) AllResolved() bool {
	return a != nil && a.Total > 0 && a.Resolved >= a.Total
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
	if hint != nil {
		adv.Resolved, adv.Total = hint.Resolved, hint.Total
	}
	adv.Chip, adv.Caption = StageTrackerAdvisoryChip, StageTrackerAdvisoryCaption
	switch {
	case adv.Empty:
		adv.Chip, adv.Caption = StageTrackerAdvisoryEmptyChip, StageTrackerAdvisoryEmptyCaption
	case adv.Working():
		adv.Chip, adv.Caption = StageTrackerAdvisoryWorkingChip(adv.Resolved, adv.Total), StageTrackerAdvisoryWorkingCaption(adv.Resolved, adv.Total)
	case adv.AllResolved():
		adv.Chip, adv.Caption = StageTrackerAdvisoryResolvedChip, StageTrackerAdvisoryResolvedCaption
	}
	return adv
}

// stageTrackerAdvisoryMods are the pills an advisory never wears: it is
// not work, so it cannot stall, be sent back, or wait on a person.
var stageTrackerAdvisoryMods = []string{modStalled, modRegressed, modNeedsHuman, modNeedsDecision}

// applyStageTrackerAdvisory marks it as an advisory: the caption and the
// actor say "read, then close" (or "hive is working the items"), the
// stall/regression/needs-human pills come off, the 000-new block (which
// would say "waiting for triage") and any 045 park are dropped along with
// the needs-decision block and blocker, and the stage actor is a human
// with nothing blocking on them.
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
	if it.Stage == stagelabels.IssueNeedsDecision.Number() {
		it.Stage, it.StageSlug = 0, ""
	}
	it.DecisionNeeded = nil
	it.Blocker = nil
	why, reason := stageTrackerAdvisoryWhy, "advisory — read, then close"
	if adv.Total > 0 {
		why, reason = stageTrackerAdvisoryWorkingWhy, "advisory checklist in progress"
	}
	it.StageActor = &StageTrackerActor{Kind: StageActorHuman, Why: why, Since: it.StageSince, HumanReason: reason}
}
