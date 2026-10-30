// Package prstage holds the shared PR lane vocabulary and observation shapes.
package prstage

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/review/reviewtext"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Detail is what a lane's deep evaluation learned beyond the reason:
// the reviewer verdict for the head and the author's admission tier.
type Detail struct {
	Verdict      ReviewVerdict
	VerdictKnown bool
	// HumanApprover is a trusted human whose APPROVED review exactly matches
	// HumanApprovalHead. It resolves a requires_human reviewer verdict without
	// overriding changes_requested/reject.
	HumanApprover     string
	HumanApprovalHead string
	// HumanChangeRequester is the trusted human whose recorded change
	// request (a CHANGES_REQUESTED review or a `/hive decision <text>` at
	// 085) stands against HumanChangeRequestHead. It supersedes a
	// requires_human / decision-requested reviewer verdict on that head:
	// the person has answered, and the PR belongs in fixing, not 085
	// (hivecommons/hive#14679).
	HumanChangeRequester   string
	HumanChangeRequestHead string
	Role                   stagelabels.Role
	// RedChecks names the completed-red, non-required checks the CI gate
	// walked past on RedChecksHead (sorted). The lane ignores them by
	// design; the stage engine must not call the PR merge-queued or
	// awaiting admission while the rollup on its head shows a red check
	// (hivecommons/hive#12538, seen on #12457).
	RedChecks     []string
	RedChecksHead string
	// Comments is the PR's issue + review comment count from the deep
	// fetch (the list payload has none); CommentsKnown says it was seen.
	// OwnComments counts the comments this engine posted after that fetch
	// (creditOwnComment); a later fetch resets it.
	Comments      int
	CommentsKnown bool
	OwnComments   int
	// Reviews is the number of review submissions by people (the hive
	// app's excluded) from a lane's ListReviews walk; ReviewsKnown says it
	// was seen this cycle.
	Reviews      int
	ReviewsKnown bool
	// DCO, DCOTotal and DCOUnsigned are the stage pass's own commit check
	// for the head (hivecommons/hive#12542); filled by ApplyStageLabels,
	// not by the lanes.
	DCO                 stagelabels.DCOState
	DCOTotal            int
	DCOUnsigned         int
	DCOUnsignedHive     int
	DCOUnsignedOther    int
	DCOSquashUnsafeHive int
	// MergeCommit says the pass elected a merge commit for the head
	// (mergeCommitElection); MergeCommitRefused names why it could not
	// when squash-unsafe hive commits are present ("" when it did or the
	// question never arose).
	MergeCommit        bool
	MergeCommitRefused string
	// DCOGate / DCOGateHead record whether the CI gate saw a DCO context
	// on the head (required-check set or reported status/check run): the
	// detected "this repo enforces DCO" signal the 072 override needs when
	// no per-repo override is configured.
	DCOGate     bool
	DCOGateHead string
}

// RedCheck returns the first red non-required check on head, or "".
func (d Detail) RedCheck(head string) string {
	if len(d.RedChecks) == 0 || (head != "" && d.RedChecksHead != "" && d.RedChecksHead != head) {
		return ""
	}
	return d.RedChecks[0]
}

// ReasonOptionalCheckFailing prefixes the recorded reason when a red
// non-required check put the PR in its repair stage.
const ReasonOptionalCheckFailing = StageReasonOptionalCheckFailing

// Observation is one lane's view of one PR in one cycle.
type Observation struct {
	Lane    string
	Display string
	Owner   string
	Repo    string
	Number  int
	HeadSHA string
	Labels  []string
	Reason  string
	// LaneReason is the lane's own skip reason before the reviewer verdict
	// or the human route replaced Reason; it is what still says whether CI
	// is red, pending or green on the head.
	LaneReason string
	Merged     bool
	Err        bool
	Detail     Detail
	// Author, Title, URL and OpenedAt feed the stage-072 contributor nudge.
	Author string
	// AuthorKind is who wrote the PR (authorkind.Detect), set on the
	// resolved observation by ApplyStageLabels before the nudge/adoption
	// hooks run: only an author/clanker PR has a hive to interrupt.
	AuthorKind authorkind.Kind
	Title      string
	URL        string
	OpenedAt   time.Time
	// BaseRef and Body feed the stage-074 adoption (superseding PR target
	// and the Closes lines to carry over); both come from the list payload.
	BaseRef string
	Body    string
	// HeadRepo is the head's owner/repo (differs from Display on a fork).
	HeadRepo string
	// HeadRef is the head branch name; with HeadRepo it names the ref an
	// in-place DCO repair rewrites (hivecommons/hive#14666).
	HeadRef string
	// ForwardMerge is set for a sync/top-up head between release lines
	// (head-ref match only: the title is contributor-controlled).
	ForwardMerge bool
	// Route is set by ApplyStageLabels when the lane handed the PR to a
	// human and the evidence routed it (feeds the entry comments and the
	// 072 nudge diagnosis).
	Route *stagelabels.Route
	// SharedCIIncident is the open shared-CI incident the head is deferred
	// to (StageLabelPolicy.SharedCIIncident), 0 when none.
	SharedCIIncident int
	// BaseRedChecks are the head's failing checks that are also red on the
	// base branch (StageLabelPolicy.BaseBranchRed); non-empty only when
	// every failing check is.
	BaseRedChecks []string
	// ForkGuardChecks are the head's failing checks that refuse the PR only
	// because it is from a fork (StageLabelPolicy.ForkGuard); non-empty only
	// when every failing check does.
	ForkGuardChecks []string
	// Hold is who set the `hold` label the PR wears and why, read by
	// ApplyStageLabels from the label event and the hive's notice comment
	// (hivecommons/hive#15020); nil when not held or unreadable.
	Hold *stagelabels.HoldProvenance
}

// Verdict is a lane's opinion about a PR's stage.
type Verdict struct {
	Stage stagelabels.Stage
	// Rank orders competing opinions; see the stageRank* constants.
	Rank int
	// Terminal means the PR left the pipeline (merged/closed/gone): remove
	// every hive-owned label and forget its timing.
	Terminal bool
	// Reason, when set, replaces the lane's skip reason as the explanation
	// recorded with the stage (store, regression/stall comments, log): the
	// lane's reason may be a CI state while the stage was really decided
	// by the reviewer verdict.
	Reason string
	// Human marks a hand-off to a person (085/090 candidate) that
	// ApplyStageLabels refines against the head's evidence
	// (stagelabels.RouteHuman) before it is written.
	Human bool
}

// Opinion ranks. A lane that cannot vouch for the author has only a weak
// opinion ("needs a human, as far as this lane can tell"): another lane may
// admit the same author, and its owned opinion wins. The contributor lane's
// admission reasons name the registry/claim tier, so they outrank the
// trusted-author lane's bare role check when both are weak.
const (
	RankNone      = 0
	RankRole      = 1
	RankAdmission = 2
	RankOwned     = 3
)

type ReviewVerdict struct {
	// Configured is false when no reviewer agent is enabled for the repo; the
	// tier then skips with contributor-lane:no-reviewer.
	Configured bool
	// Approved is true when the aggregate verdict for the head is approve and
	// merge-eligible.
	Approved bool
	// Verdict is the aggregate verdict for routing, if any.
	Verdict           string
	DecisionRequested bool
	DecisionQuestion  string
	// DecisionFinding names what the review found that makes the decision
	// a person's (duplicate, risk-class question, finding at the human
	// threshold, reviewer high-risk marker). A decision-requested verdict
	// without one never reaches the stage engine: the aggregate resolves
	// it as advisory (review.AggregateReports, owner rule 2026-10-29).
	DecisionFinding string
	// RedundantOf is the PR the reviewer named as already carrying this
	// change (`<!-- hive-redundant-of: #N -->`): the decision is a close,
	// and for the hive's own PR the lane performs it (hivecommons/hive#15499).
	RedundantOf    int
	HumanSummaries []string
	// HiveAuthoredKnown is true for verdicts read from aggregates that recorded
	// authorship. The stage fallback normalizer leaves those alone: aggregation
	// already applied the configured self-authored decision policy.
	HiveAuthoredKnown bool
	HiveAuthored      bool
	// FixCycleCapReached preserves a human hold once review repair attempts are
	// exhausted, even if the review text also names concrete remediation.
	FixCycleCapReached bool
	// DecisionAnswered is set when a trusted human answered this head's
	// requires_human / decision-requested verdict — a `/hive decision` or a
	// CHANGES_REQUESTED review recorded against DecisionAnsweredHead
	// (hivecommons/hive#14679). The question is no longer open: a
	// DecisionDirective of changes_requested routes to fixing with the
	// instruction, approve lets the lane proceed. A new head produces a new
	// verdict with no answer, so the stage asks again.
	DecisionAnswered     bool
	DecisionAnsweredBy   string
	DecisionAnsweredHead string
	DecisionDirective    string
}

// Decision directives, mirroring review.DecisionDirective*.
const (
	DecisionDirectiveApprove = "approve"
	DecisionDirectiveChanges = "changes_requested"
)

// WithDecisionAnswer returns v with the human's answer on head folded in.
func (v ReviewVerdict) WithDecisionAnswer(by, head, directive string) ReviewVerdict {
	by, head = strings.TrimSpace(by), strings.TrimSpace(head)
	if by == "" || head == "" {
		return v
	}
	v.DecisionAnswered, v.DecisionAnsweredBy, v.DecisionAnsweredHead = true, by, head
	if strings.TrimSpace(directive) == DecisionDirectiveApprove {
		v.DecisionDirective = DecisionDirectiveApprove
	} else {
		v.DecisionDirective = DecisionDirectiveChanges
	}
	return v
}

func NormalizeReviewVerdict(v ReviewVerdict) ReviewVerdict {
	if !v.Configured || v.Approved || v.DecisionRequested || (v.HiveAuthoredKnown && !v.HiveAuthored) || v.FixCycleCapReached || strings.ToLower(strings.TrimSpace(v.Verdict)) != "requires_human" {
		return v
	}
	text := strings.TrimSpace(v.DecisionQuestion + "\n" + strings.Join(v.HumanSummaries, "\n"))
	if reviewtext.RiskEscalationText(text) || !reviewtext.ConcreteRemediationText(text) {
		return v
	}
	v.Verdict = "changes_requested"
	return v
}
