package dashboard

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// One vocabulary for every operator-facing string the Activity Monitor
// renders (event lines, captions, popovers, toasts, pills): a stage is
// named by the phrase its chevron uses, never by its code
// (hivecommons/hive#15096 roll 3: the event line read "open PR #15118
// covers it (PR at 060 reviewed)" under a chevron saying "Awaiting merge
// gate"). 060 reads "Review passed · <lane reason>" ("CI in progress",
// "checks unreadable", "soak 30m remaining") in the CI / Review column. The SPA's STAGE_TRACKER_ACTIVE_WORD / stageTrackerAnchor carry
// the same column and word tables; TestStageTrackerVocabularyTable pins
// every label in stagelabels to a column and a phrase.

// Tracker columns (the main strip's chevrons), by the stage number that
// heads each.
const (
	trackerColumnOpen       = 0
	trackerColumnActionable = trackerNumberActionable
	trackerColumnClaimed    = trackerNumberClaimed
	trackerColumnPR         = trackerNumberPRInProgress
	trackerColumnReview     = trackerNumberReviewPending
	trackerColumnMergeGate  = trackerNumberAwaitingMerge
	trackerColumnMerged     = 110
)

// stageTrackerColumn is the chevron a PR row at stage lights. The ladder
// is 050 review-pending → 060 reviewed → 070 fixing → 080 awaiting merge
// admission → 085/090 → 095 soak → 100 merge queue: 060 is pre-admission
// (CI still pending or red; it can regress to 070), so it stays in the
// CI / Review column — only 080 and later are the merge gate.
func stageTrackerColumn(stage stagelabels.Stage) int {
	switch stage {
	case stagelabels.Actionable, stagelabels.TriageNeeded, stagelabels.PossiblyExisting, stagelabels.IssueNeedsDecision:
		return trackerColumnActionable
	case stagelabels.Claimed:
		return trackerColumnClaimed
	case stagelabels.PRInProgress, stagelabels.CoveredByPR:
		return trackerColumnPR
	case stagelabels.ReviewPending, stagelabels.Reviewed, stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.AdoptedByHive:
		return trackerColumnReview
	case stagelabels.AwaitingMergeAdmission, stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge, stagelabels.PreMergeSoak, stagelabels.MergeQueue:
		return trackerColumnMergeGate
	}
	return trackerColumnOpen
}

// stageTrackerItemColumn is the chevron a PR row lights given its CURRENT
// state — never the highest stage it once reached. The ladder is:
//
//   - merged (≥ 110) → Merged;
//   - under repair past 030 (stageTrackerTrainUnderRepair: 070/071/072/
//     074, or red required checks) → CI / Review, whatever a stale merge
//     gate ("merge-failed" from the attempt that found the conflict) or a
//     planner-ordered merge train still says — except the 085 decision
//     park, which stays at the gate where the person decides;
//   - a live merge attempt (merging / merge-failed past 030) or a seated
//     train member → Merge queue (hivecommons/docs#512, hive#15395);
//   - otherwise the stage's own column (stageTrackerColumn).
//
// The "fell back to …" caption keeps the history; the column follows the
// fallback. The SPA's stageTrackerAnchor applies the same order.
func stageTrackerItemColumn(it *StageTrackerItem) int {
	if it == nil {
		return trackerColumnOpen
	}
	if it.Stage >= trackerColumnMerged {
		return trackerColumnMerged
	}
	st := stageByNumber(it.Stage)
	if it.Kind == "pr" && it.Stage > trackerColumnPR && st != stagelabels.NeedsDecision && stageTrackerTrainUnderRepair(it.Stage, it.Reason) != "" {
		return trackerColumnReview
	}
	if it.Stage > trackerColumnPR {
		if mg := it.MergeGate; mg != nil && mg.State == MergeGateStateMergeFailed && mg.MergeFailedKind == MergeRefusalChecksPending &&
			(st == stagelabels.ReviewPending || st == stagelabels.Reviewed) {
			return trackerColumnReview
		}
		if mg := it.MergeGate; mg != nil && (mg.State == MergeGateStateMerging || mg.State == MergeGateStateMergeFailed) {
			return trackerColumnMergeGate
		}
		if mt := it.MergeTrain; mt != nil && mt.State != "" {
			return trackerColumnMergeGate
		}
	}
	return stageTrackerColumn(st)
}

// stageTrackerStagePhrase is the stage as a lowercase clause for the
// middle of a sentence ("its PR is reviewed · waiting for merge
// admission"), from the same words the chevron shows.
func stageTrackerStagePhrase(stage stagelabels.Stage) string {
	switch stage {
	case stagelabels.Actionable:
		return "actionable"
	case stagelabels.TriageNeeded:
		return "waiting for triage"
	case stagelabels.PossiblyExisting:
		return "waiting for the author to confirm the gap (prior art found)"
	case stagelabels.Claimed:
		return "claimed"
	case stagelabels.PRInProgress:
		return "a PR in progress"
	case stagelabels.CoveredByPR:
		return "covered by a PR"
	case stagelabels.IssueNeedsDecision, stagelabels.NeedsDecision:
		return "waiting for a human decision"
	case stagelabels.ReviewPending:
		return "in CI / review"
	case stagelabels.Reviewed:
		return "review passed · waiting for merge admission"
	case stagelabels.Fixing:
		return "being fixed by hive"
	case stagelabels.NeedsAuthor:
		return "waiting for its author"
	case stagelabels.ReturnedToContributor:
		return "returned to the contributor"
	case stagelabels.AdoptedByHive:
		return "adopted by hive"
	case stagelabels.AwaitingMergeAdmission:
		return "at the merge queue"
	case stagelabels.ReadyForHumanMerge:
		return "ready for human merge"
	case stagelabels.PreMergeSoak:
		return "soaking before merge"
	case stagelabels.MergeQueue:
		return "in the merge queue"
	}
	return ""
}

// stageTokenRE matches a stage code however a lane reason spells it:
// `stage/060-reviewed`, `060-reviewed`, `060 reviewed`.
var stageTokenRE = regexp.MustCompile(`(?:stage/)?\b(0\d{2}|1[01]0)[- ]([a-z][a-z-]*[a-z])\b`)

// stageTrackerPlainStages rewrites every stage code in s to its phrase, so
// a reason the lane wrote for its own log reads in the chevron's words.
func stageTrackerPlainStages(s string) string {
	return stageTokenRE.ReplaceAllStringFunc(s, func(tok string) string {
		m := stageTokenRE.FindStringSubmatch(tok)
		if stage, ok := stagelabels.StageBySlug(strings.ReplaceAll(m[2], " ", "-")); ok {
			if p := stageTrackerStagePhrase(stage); p != "" {
				return p
			}
		}
		return tok
	})
}

// coverReasonRE is the issue lane's cover reason: "open PR #99 covers it
// (PR at 060-reviewed)".
var coverReasonRE = regexp.MustCompile(`^open PR #(\d+) covers it \(PR at ((?:0\d{2}|1[01]0)-[a-z-]+)\)$`)

// humanizeCoverReason turns the cover reason into the clause that follows
// "An open PR #99 covers this issue —": what the PR is doing now, once,
// in the chevron's words. "" for a reason that is not a cover reason.
func humanizeCoverReason(reason string) (string, bool) {
	m := coverReasonRE.FindStringSubmatch(strings.TrimSpace(reason))
	if m == nil {
		return "", false
	}
	stage, _ := stagelabels.StageBySlug(strings.TrimPrefix(m[2], m[2][:4]))
	if p := stageTrackerStagePhrase(stage); p != "" {
		return "its PR is " + p, true
	}
	return "", true
}
