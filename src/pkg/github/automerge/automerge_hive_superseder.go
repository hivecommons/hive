package automerge

// The owner's superseder rule (@clubanderson): "if a pr is superseded by a
// pr that a hive agent creates, it should auto merge" — "the only exception
// would be if this is sentinel or some other emergent problem with the pr".
// A hive-authored superseding PR therefore never parks at 085 on an
// ordinary reviewer requires_human (the aggregate routes those to the
// fixer, review.hiveSupersedingFixRoute); hiveSupersederHardStop is the
// whole list of what does park it, and the 085 caption names which.

import (
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hiveSupersedingPR reports whether obs is the hive App's own superseding
// copy of another PR under the (default-on) auto_merge.hive_superseding
// switch: hive-authored and declaring "Supersedes #N" in its body.
func (c *Engine) hiveSupersedingPR(obs stageObservation) bool {
	if c != nil && c.hiveSupersedingAutoMerge != nil && !c.hiveSupersedingAutoMerge() {
		return false
	}
	hiveAuthored := obs.AuthorKind == authorkind.Hive || obs.Lane == stageLaneSelf
	return hgithub.HiveSupersedingPR(hiveAuthored, obs.Body)
}

// sentinelAcknowledgedFor returns who acknowledged the sentinel's findings
// on the head ("" when nobody has): the configured hook, else the review
// evidence bundle's newest sentinel_acknowledged human action.
func (c *Engine) sentinelAcknowledgedFor(repo string, number int, head string) string {
	if c != nil && c.sentinelAcknowledgedBy != nil {
		return strings.TrimSpace(c.sentinelAcknowledgedBy(repo, number, head))
	}
	if strings.TrimSpace(head) == "" {
		return ""
	}
	b, err := hgithub.LoadReviewEvidence(hgithub.ReviewEvidencePath("", repo, number, head))
	if err != nil || b == nil {
		return ""
	}
	return strings.TrimSpace(b.SentinelAcknowledgedBy())
}

// hiveSupersederHardStop is the one exception list for a hive-authored
// superseding PR. It returns the stagelabels.HiveSupersederHardStop* token
// and true when the PR must park at 085:
//
//   - the sentinel has a finding on the PR (its label is on) and nobody
//     has acknowledged it (sentinelAckBy == "");
//   - a hold is set (`hold`, `do-not-merge/*`, a repo/global hold);
//   - the fixer's attempts are exhausted (FixCycleCapReached), reported as
//     CI red / base conflict when that is what is still wrong, else as
//     fix attempts exhausted;
//   - the base conflict is one the hive already failed to resolve
//     (rebase-failed-twice).
//
// An ordinary reviewer finding, a pending check or a conflict the fixer has
// not yet been asked to rebase is NOT a hard stop: those are the fixer's.
func hiveSupersederHardStop(e stagelabels.Evidence, verdict ContributorLaneVerdict, sentinelAckBy string) (reason string, ok bool) {
	switch {
	case e.Sentinel && strings.TrimSpace(sentinelAckBy) == "":
		return stagelabels.HiveSupersederHardStopSentinel, true
	case len(e.Holds) > 0 || e.Hold != nil:
		return stagelabels.HiveSupersederHardStopHold, true
	case verdict.FixCycleCapReached && e.Checks.Known && len(e.Checks.Red) > 0:
		return stagelabels.HiveSupersederHardStopCIRed, true
	case verdict.FixCycleCapReached && e.Conflict:
		return stagelabels.HiveSupersederHardStopConflict, true
	case verdict.FixCycleCapReached:
		return stagelabels.HiveSupersederHardStopFixExhausted, true
	case e.Conflict && conflictUnresolvable(e):
		return stagelabels.HiveSupersederHardStopConflict, true
	}
	return "", false
}

// conflictUnresolvable says the conflict already defeated the hive's
// rebase-resolve episode (both attempts spent).
func conflictUnresolvable(e stagelabels.Evidence) bool {
	for _, r := range []string{e.LaneReason, e.CauseReason} {
		r = strings.TrimSpace(r)
		if r == stagelabels.LaneReasonRebaseFailedTwice || strings.HasSuffix(r, stagelabels.LaneReasonAdoptionStalledPrefix+stagelabels.LaneReasonRebaseFailedTwice) {
			return true
		}
	}
	return false
}
