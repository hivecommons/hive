package prstage

import (
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An adopted PR is the hive's own fixer's to repair only while the hive can
// push its branch. projectbluefin/utah#919 (a fork the hive recorded
// fork-rehome-refused on) and projectbluefin/testsuite#1037 (a fork whose
// diff changes .github/workflows/e2e.yml; the fixer reported three times
// that GitHub refused its push) were both decided "hive's fixer: ... then
// it merges" and read "the hive is fixing it" for days while nothing could
// be pushed. The dashboard's delegate and the PR-decision relay read the
// same answer from AdoptedPushFacts.Refusal, so they cannot disagree on
// whether the hive can push.

// AdoptStallForkRehomeRefused is the adoption-stalled suffix for a fork PR
// the hive would not move into the base repository (the adoption engine's
// adoptStallForkRehomeRefused).
const AdoptStallForkRehomeRefused = "fork-rehome-refused"

// AdoptedPushFacts is what says whether the hive can push an adopted PR's
// branch: the PR's shape and what the hive recorded trying.
type AdoptedPushFacts struct {
	Fork                bool
	MaintainerCanModify bool
	// WorkflowPath: the PR's diff against the base changes
	// .github/workflows/ (known only where the files were listed).
	WorkflowPath bool
	// RehomeRefused: the adoption engine recorded fork-rehome-refused on
	// this head (it can neither push the fork in place nor supersede it).
	RehomeRefused bool
	// Escalation is the adoption-stalled reason recorded on this head, if
	// any; Capability is the short text of a capability blocker the ledger
	// holds for the item ("App lacks workflows permission").
	Escalation string
	Capability string
	// Adopted: the stage store says the hive took this PR on (a live
	// adoption, an escalated one at this head, or a refused re-home).
	Adopted bool
}

// FromRecord fills what the stage store holds about pushing the PR at head.
// The blocker ledger's Capability is the caller's to add.
func (f *AdoptedPushFacts) FromRecord(rec stagelabels.Record, head string) {
	f.RehomeRefused = rec.ForkRehomeRefused(head)
	if rec.AdoptionEscalated(head) {
		f.Escalation = strings.TrimSpace(rec.AdoptEscalatedReason)
	}
	f.Adopted = rec.AdoptionLive(head) || rec.AdoptionEscalated(head) || f.RehomeRefused
}

// Refusal names why the hive cannot push the branch; "" when it can.
func (f AdoptedPushFacts) Refusal() string {
	esc := strings.ToLower(f.Escalation)
	switch {
	case f.RehomeRefused || strings.HasSuffix(esc, AdoptStallForkRehomeRefused):
		return "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	case f.Fork && !f.MaintainerCanModify:
		return "the fork does not allow edits by maintainers"
	case f.Fork && f.WorkflowPath:
		return "the PR changes .github/workflows/, which the hive cannot push to a fork"
	case strings.Contains(esc, ContributorLaneReasonWorkflowPath):
		return "the adoption stalled because the PR changes .github/workflows/ (" + f.Escalation + ")"
	case f.Capability != "":
		return "the hive's fixer reported it cannot push: " + f.Capability
	}
	return ""
}

// AdoptionEscalationOutlastsDecision reports whether an adoption escalation
// recorded on a head keeps the PR at 085 even after a maintainer's
// direction on that head, so the relay must not promise the hive's fixer
// (projectbluefin/utah-packages#1309 follow-up). These are the reasons the
// adoption engine's escapeStalledAdoption holds or re-derives before the
// stale clock: a verdict on the head itself (the fixer's push undid the
// PR, the copy failed or its child closed twice, the workflow-path
// refusal) or a structural stall (fork-rehome-refused, supersede-failed,
// the hive's sign-off attribution, a copy that failed its rebase twice).
// Only a plain idle stall, "adoption-stalled:<lane reason>", is released
// by a decision; any reason this does not recognise counts as held.
func AdoptionEscalationOutlastsDecision(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return false
	}
	stall, ok := strings.CutPrefix(reason, stagelabels.LaneReasonAdoptionStalledPrefix)
	if !ok {
		// adoption-fixer-reverts-pr, adoption-child-closed-twice,
		// adoption-copy-failed and anything else not an idle stall.
		return true
	}
	switch {
	case stall == "",
		stall == ContributorLaneReasonWorkflowPath,
		stall == AdoptStallForkRehomeRefused,
		strings.HasPrefix(stall, "supersede-failed:"),
		stall == stagelabels.LaneReasonHiveSignoffAttribution,
		stall == stagelabels.LaneReasonRebaseFailedTwice:
		return true
	}
	return false
}
