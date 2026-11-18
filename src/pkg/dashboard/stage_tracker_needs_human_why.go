package dashboard

import (
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// One state for "blocked on a person, not on an agent". The tracker already
// had several pieces of it (the needs-human pill, 045/085 decisions, 015
// triage, 090 human merge, the unauthorized pill); an issue an agent filed
// for outside help, and a PR held by a hive gate label, showed up as a
// stalled "Open · since 4h" row or as a plain hold instead. The payload now
// carries needs_human_why on every row a person must move, naming which
// kind of wait it is and the one-line reason, and sets needs_human, so the
// chip, the count, the filter and the waiting-on-you list all agree. The
// phrase is "Needs human" everywhere (hivecommons/hive: owner asked for one
// consistent wording). Nothing here is a setting or a label somebody has to
// apply: every source is read from data the hive already holds.

// Sources of a Needs human wait (StageTrackerNeedsHumanWhy.Source).
const (
	// needsHumanSourceFiledForHelp: an agent filed the issue for outside
	// help (the body's filed-for-help marker) and nobody has claimed it.
	needsHumanSourceFiledForHelp = "filed-for-help"
	// needsHumanSourceDecision: a decision was requested and is unanswered.
	needsHumanSourceDecision = "decision-pending"
	// needsHumanSourceLevelGate: the PR is held for a maintainer by the
	// hive's ACMM level gate (the ready-for-review gate label).
	needsHumanSourceLevelGate = "level-gate"
	// needsHumanSourceHiveHold: the hive itself placed a hold that only a
	// person lifts (reporter trust and similar gates).
	needsHumanSourceHiveHold = "hive-hold"
	// needsHumanSourceApproval: a person's approval is the next step (the
	// App lacks a permission, a human must merge).
	needsHumanSourceApproval = "needs-approval"
	// needsHumanSourceTriage: waiting for a maintainer's triage.
	needsHumanSourceTriage = "triage"
	// needsHumanSourceAuthor: the PR waits on its human author.
	needsHumanSourceAuthor = "author"
	// needsHumanSourceMarker: the item wears the needs-human marker.
	needsHumanSourceMarker = "needs-human"
)

// StageTrackerNeedsHumanChip is the one phrase every surface uses for this
// state: the row badge, tooltips, the filter chip, docs and the API.
const StageTrackerNeedsHumanChip = "Needs human"

// StageTrackerNeedsHumanWhy is the payload block on a row that waits on a
// person: what kind of wait, and the reason in one short line.
type StageTrackerNeedsHumanWhy struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Reason lines. Short, calm, no blame.
const (
	needsHumanReasonDecisionUnanswered = "decision requested, no answer yet"
	needsHumanReasonLevelGate          = "held for a maintainer by the level gate; review it, then remove the label"
	needsHumanReasonApprovalPerm       = "needs approval: the hive bot lacks permission for its next step"
	needsHumanReasonApprovalMerge      = "needs approval: a maintainer merges it"
	needsHumanReasonTriage             = "waiting for a maintainer's triage"
	needsHumanReasonAuthor             = "waiting on the PR author"
	needsHumanReasonMarker             = "marked needs-human; a person must act"
	needsHumanReasonHiveHold           = "held by the hive until a maintainer lifts it"
)

// stageTrackerFiledForHelpReason is the one-line reason of a filed-for-help
// issue, naming the filing agent.
func stageTrackerFiledForHelpReason(agent string) string {
	return "filed for help by " + agent + ": needs a person outside the hive"
}

// stageTrackerGateLabel returns the hive gate label the item wears, "" when
// none, and whether it is the level gate's (ready-for-review) rather than a
// decision gate.
func stageTrackerGateLabel(labels []string) (label string, levelGate bool) {
	for _, l := range labels {
		if !github.IsHiveGateLabel(l) {
			continue
		}
		t := strings.TrimSpace(l)
		isLevel := strings.EqualFold(t, github.ReadyForReviewLabelName()) || strings.EqualFold(t, github.DefaultReadyForReviewLabel)
		if isLevel {
			return t, true
		}
		label = t
	}
	return label, false
}

// stageTrackerNeedsHumanWhyFor derives the row's Needs human state from
// what the tracker already computed; nil means the row waits on an agent (or
// nobody). Pure: no I/O, so the derivation is table-testable.
func stageTrackerNeedsHumanWhyFor(it StageTrackerItem) *StageTrackerNeedsHumanWhy {
	// An advisory asks for nothing, a decided row is the hive's to act on
	// next, a draft is its author's and a merged row is done.
	if it.Advisory != nil || it.Decided != nil || slices.Contains(it.Modifiers, modDraft) || slices.Contains(it.Modifiers, "merged") {
		return nil
	}
	if gate, level := stageTrackerGateLabel(it.Labels); gate != "" {
		if level {
			return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceLevelGate, Reason: needsHumanReasonLevelGate}
		}
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceDecision, Reason: stageTrackerDecisionReason(it)}
	}
	held := stageTrackerOnHold(it)
	if it.Kind == "issue" && it.FiledForHelp != "" && !it.Mirrored && it.Stage < trackerNumberClaimed && !held {
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceFiledForHelp, Reason: stageTrackerFiledForHelpReason(it.FiledForHelp)}
	}
	if it.BlockedOwnerReason != "" && !held {
		// A `blocked` issue whose blocker is a person's action (the
		// owner-action blocked-on marker): the reason line is already
		// "needs an owner action: ...".
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceBlockedOnOwner, Reason: it.BlockedOwnerReason}
	}
	if held {
		// A hold a person set is their own pause and waits on nobody; one
		// the hive placed waits on a maintainer to lift it.
		if it.Hold != nil && strings.EqualFold(strings.TrimSpace(it.Hold.By), "hive") {
			reason := strings.TrimSpace(it.Hold.Pill)
			if reason == "" {
				reason = needsHumanReasonHiveHold
			}
			return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceHiveHold, Reason: reason}
		}
		return nil
	}
	if !stageTrackerNeedsHuman(it) {
		return nil
	}
	switch {
	case it.Stage == stagelabels.NeedsDecision.Number(), it.Stage == stagelabels.IssueNeedsDecision.Number(), slices.Contains(it.Modifiers, modNeedsDecision):
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceDecision, Reason: stageTrackerDecisionReason(it)}
	case slices.Contains(it.Modifiers, modUnauthorized):
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceApproval, Reason: needsHumanReasonApprovalPerm}
	case it.Stage == stagelabels.ReadyForHumanMerge.Number():
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceApproval, Reason: needsHumanReasonApprovalMerge}
	case it.Stage == stagelabels.TriageNeeded.Number():
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceTriage, Reason: needsHumanReasonTriage}
	case slices.Contains(stageTrackerNeedsHumanAuthorStages, it.Stage):
		return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceAuthor, Reason: needsHumanReasonAuthor}
	}
	return &StageTrackerNeedsHumanWhy{Source: needsHumanSourceMarker, Reason: needsHumanReasonMarker}
}

// stageTrackerDecisionReason is the question a pending decision asks, else
// the generic line.
func stageTrackerDecisionReason(it StageTrackerItem) string {
	if d := it.DecisionNeeded; d != nil {
		if q := strings.TrimSpace(d.Question); q != "" {
			return q
		}
	}
	return needsHumanReasonDecisionUnanswered
}
