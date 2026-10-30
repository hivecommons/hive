package stagelabels

// Routing of "a human must act" (hivecommons/hive#12528). A lane that
// cannot merge a PR itself used to park it at one stage, 090-needs-human,
// and the maintainer's question — "should I click merge?" — was answered
// by reading the swarm verdict, CI, DCO, the sentinel label, the soak timer
// and the hold modifiers by hand. The stage now answers it:
//
//   - 071 needs-author / 072 returned-to-contributor (070 fixing for a hive
//     PR; FixingStageFor picks by author kind): the head is not
//     mergeable on its merits — verdict changes_requested, red checks, DCO
//     missing, conflicts. The author acts.
//   - 050 review-pending: no verdict for this head yet (or the head was just
//     pushed and nothing has reported). Nobody acts; wait.
//   - 085 needs-decision: a person must decide something other than the
//     merge — sentinel alert, hold / do-not-merge / needs-decision label, or
//     a lane rule that is human-only for a non-trust reason.
//   - 060 reviewed: approve verdict, but checks or the soak are still
//     running. Wait.
//   - 090 ready-for-human-merge: everything the hive would need is in place
//     and only trust/tier/role stops it. The maintainer's /lgtm /approve is
//     the whole remaining job.
//
// RouteHuman is a pure function of Evidence so every rule is testable.

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
)

// VerdictState is the reviewer swarm's verdict for the CURRENT head.
type VerdictState int

const (
	// VerdictPending: no verdict for this head yet (or only for an older head).
	VerdictPending VerdictState = iota
	VerdictApproved
	VerdictChangesRequested
	// VerdictUnavailable: no reviewer is configured, so no verdict will
	// come; unreviewed code is a human decision, not a human merge.
	VerdictUnavailable
	// VerdictRequiresHuman: the swarm answered requires_human for this head.
	// A person owes a decision (085), not a review wait (050).
	VerdictRequiresHuman
)

// DCOState is what the DCO context said about the head.
type DCOState int

const (
	// DCOUnknown: the head carries no DCO context (the repo may not run
	// one). It neither blocks nor is claimed as "yes".
	DCOUnknown DCOState = iota
	DCOYes
	DCONo
)

// CheckSummary is the CI picture for the head with tide and the other
// merge-metadata contexts already excluded.
type CheckSummary struct {
	// Total counts gating checks observed on the head; Green those that
	// completed without failing; Pending those still running or queued.
	Total, Green, Pending int
	// Red names every failing gating check, in a stable order.
	Red []string
	// Known is false when the head could not be read (API error): CI is
	// then a pending gate, never a pass.
	Known bool
}

// HumanCause is why the lane handed the PR to a human.
type HumanCause int

const (
	// CauseTrust: the lane cannot vouch for the author (tier below
	// require_tiers, no claim / registry profile, no trusted-author role).
	// Everything else may be fine — this is the 090 candidate.
	CauseTrust HumanCause = iota
	// CauseDecision: a human-only lane rule unrelated to trust (hold /
	// excluded label, sentinel, workflow-path edits, tests removed, no
	// reviewer, reporter-trust hold, human-merge path, …).
	CauseDecision
)

// Evidence is everything RouteHuman decides from.
type Evidence struct {
	// Head is the current head SHA (rendered short in comments).
	Head string
	// HeadFresh is true when nothing has reported on the head yet: it was
	// just pushed, so the hive withholds an opinion.
	HeadFresh bool
	Verdict   VerdictState
	Checks    CheckSummary
	DCO       DCOState
	// Sentinel is true when the sentinel alert label is on the PR.
	Sentinel bool
	// Conflict is true when the head does not merge cleanly (lane reason,
	// needs-rebase label).
	Conflict bool
	// Soak is the lane's soak window (0 = none); SoakElapsed how long the
	// head has been fully green.
	Soak, SoakElapsed time.Duration
	// Holds lists the decision-class modifier labels present, lower-cased
	// (see HoldLabels).
	Holds []string
	// Hold is who set the `hold` label and why, when the pass could read
	// it (hivecommons/hive#15020); the 085 reason then names them instead
	// of the bare "the `hold` label is set".
	Hold *HoldProvenance
	// Cause is why the lane handed off; CauseTrust is the only one that can
	// end at 090.
	Cause HumanCause
	// CauseReason is the lane's reason string, rendered when Cause is a
	// decision the comment has to name.
	CauseReason string
	// DecisionQuestion is the maintainer-facing question from the reviewer
	// verdict/comment, when the swarm explicitly deferred a product call.
	DecisionQuestion string
	// DecisionFinding is what the review found that makes the question a
	// person's — printed so the comment and tracker say WHY, not just that
	// a decision is owed (hivecommons/hive, owner rule 2026-10-29).
	DecisionFinding string
	// HumanSummaries are reviewer summaries for requires_human verdicts that
	// did not carry a structured decision question.
	HumanSummaries []string
	// SelfAuthored selects 070 fixing for the "author must act" outcome;
	// otherwise AuthorKind picks 072 (clanker) or 071 (human / unknown).
	SelfAuthored bool
	AuthorKind   authorkind.Kind
	// ForkGuard names the red checks that refuse the PR only because it is
	// from a fork (a fork-publication or fork-secrets guard). They are not
	// the author's to fix: only a maintainer can clear them, so they route
	// to 085 instead of the fixing stage (hivecommons/hive#14273).
	ForkGuard []string
	// HumanApprover is the trusted merger whose GitHub APPROVED review sits
	// on exactly Head ("" when none). It is the human decision on a
	// requires_human verdict and on a manual-merge lane rule: the person
	// read the reviewer's objection and ruled the head lands as-is.
	HumanApprover string
	// HumanChangeRequester is the trusted maintainer whose CHANGES_REQUESTED
	// review (or `/hive decision <text>`) stands on exactly Head ("" when
	// none). A person has reviewed this head and asked for a fix: that
	// answers every "needs a human" park on it — the sentinel alert, a
	// workflow-path fence, a hold — so the PR is the author's to act on
	// (071 / 072 / 070), not 085 (hivecommons/hive#15159: a clanker PR
	// with the owner's changes-requested review kept re-deriving
	// "Sentinel alert · needs human" over the operator's 072).
	HumanChangeRequester string
	// ManualMergeRule is true when CauseReason names a lane rule no
	// approval can override — the hive never merges the head unattended
	// (CI workflow / action edits, removed tests, a human-merge path). With
	// HumanApprover set and nothing else outstanding the PR is at 090:
	// the only step left is the merge click, not an open-ended decision.
	ManualMergeRule bool
	// LaneReason is the lane's raw reason token behind CauseReason
	// ("contributor-lane:workflow-path"); the 090 route for an approved
	// manual-merge rule records it suffixed with the approver.
	LaneReason string
}

// HumanApprovedReasonSuffix joins a lane reason to the trusted merger who
// approved the head: "contributor-lane:workflow-path:human-approved@octocat".
const HumanApprovedReasonSuffix = ":human-approved@"

// HumanApprovedReason splits a reason carrying HumanApprovedReasonSuffix
// into the bare lane reason and the approver's login ("" when none).
func HumanApprovedReason(reason string) (base, approver string) {
	reason = strings.TrimSpace(reason)
	i := strings.LastIndex(reason, HumanApprovedReasonSuffix)
	if i < 0 {
		return reason, ""
	}
	return reason[:i], strings.TrimSpace(reason[i+len(HumanApprovedReasonSuffix):])
}

// HoldLabels are the modifier labels that turn a human hand-off into a
// decision (085). `do-not-merge/*` (prow's family) matches by prefix.
// `needs-decision` is retired (hivecommons/hive#12683; nothing writes it)
// and stays here only so a PR still carrying it keeps its decision routing
// until the label is gone.
var HoldLabels = []string{"hold", "do-not-merge", "needs-decision"}

// Holds filters labels down to the decision-class modifiers, lower-cased.
func Holds(labels []string) []string {
	var out []string
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		for _, h := range HoldLabels {
			if l == h || strings.HasPrefix(l, h+"/") {
				out = append(out, l)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Route is RouteHuman's answer.
type Route struct {
	Stage Stage
	// Actions names the actual release steps supplied by the merge lane
	// (hivecommons/hive#13087).
	Actions []string
	// ManualMerge means this lane has no unattended way past its rule.
	ManualMerge bool
	// Reasons explains the stage: what the author must fix (072/070), what
	// a person must decide (085), what is still running (060). Empty for a
	// trust-blocked 090; an approved manual-merge 090 names the rule and
	// the approver.
	Reasons []string
	// LaneReason is a reason in the lanes' vocabulary for the 072/070 case
	// ("check-failing:<name>", "conflicting", …) so the contributor nudge
	// and the stall note read as if the lane had said it. Empty otherwise.
	LaneReason string
	// Summary is the one-line evidence digest for comments:
	// "verdict approved @abc1234, 55/55 checks green, DCO yes, soak 2h elapsed".
	Summary string
	// Head, SelfAuthored and Verdict carry the evidence DecisionComment
	// needs to name the concrete actions a maintainer has on a hive-authored
	// PR parked on `requires_human` (hivecommons/hive#13041).
	Head         string
	SelfAuthored bool
	Verdict      VerdictState
	// HumanApprover is the trusted merger whose approval on Head settled
	// the requires_human verdict / manual-merge rule ("" when none).
	HumanApprover string
	// Hold carries the `hold` provenance into the 085 comment so it can
	// name who set the hold and offer release / keep (hivecommons/hive#15020).
	Hold *HoldProvenance
}

// Lane-vocabulary reasons RouteHuman emits for the fixing stages.
const (
	LaneReasonChangesRequested = "reviewer-verdict:changes_requested"
	// LaneReasonHumanChangesRequested is a trusted maintainer's standing
	// change request on the head (prstage.AutoMergeReasonHumanChangesRequested
	// aliases this constant; prstage imports this package).
	LaneReasonHumanChangesRequested  = "human-changes-requested"
	LaneReasonDCOMissing             = "dco-missing"
	LaneReasonHiveSignoffAttribution = "hive-signoff-attribution"
	LaneReasonConflicting            = "conflicting"
	// LaneReasonReviewerQuestion is the 071 reason when the reviewer's
	// requires_human verdict lands on a PR a person pushed themselves
	// (hivecommons/hive#15202): the human the reviewer wants is the author,
	// so the question goes to them — not to a decision queue, and never to
	// adoption.
	LaneReasonReviewerQuestion = "reviewer-verdict:requires_human"
	laneReasonCheckFailing     = "check-failing:"
	// LaneReasonSupersededByMerge is the ledger reason when the hive closes
	// its own PR because a merged PR superseded it (the issue it was for is
	// closed, or the merged PR declares it) instead of parking it at 085 on
	// a reviewer's requires_human (hivecommons/hive#15208).
	LaneReasonSupersededByMerge = "superseded-by-merge"
	// LaneReasonAdoptionStalledPrefix opens the 085 reason for an adopted
	// PR (074) whose head sat past the stage dwell with nothing the hive
	// could push; the suffix is the lane reason it was stuck on
	// ("adoption-stalled:reviewer-verdict:changes_requested").
	LaneReasonAdoptionStalledPrefix = "adoption-stalled:"
	// LaneReasonRebaseFailedTwice is the 085 reason for the hive's copy of
	// an adopted PR (hive/adopt-<n>) that opened conflicting and stayed
	// dirty after the fixer's rebase-resolve episode used both attempts
	// (hivecommons/hive#15104 guard); the same suffix under
	// LaneReasonAdoptionStalledPrefix parks a still-open original.
	LaneReasonRebaseFailedTwice = "rebase-failed-twice"
)

// RouteHuman decides which stage a lane's "a human must act" resolves to.
func RouteHuman(e Evidence) Route {
	fixing := FixingStageFor(e.SelfAuthored, e.AuthorKind)
	summary := summarize(e)
	var fix []string
	var laneReason string
	note := func(reason, lane string) {
		fix = append(fix, reason)
		if laneReason == "" {
			laneReason = lane
		}
	}
	if requester := strings.TrimPrefix(strings.TrimSpace(e.HumanChangeRequester), "@"); requester != "" {
		// A person reviewed this head and asked for changes: the sentinel's
		// or the lane rule's "needs a human" is answered for it, and the
		// author acts next (hivecommons/hive#15159). A new head re-asks.
		note("@"+requester+" requested changes @"+ShortSHA(e.Head), LaneReasonHumanChangesRequested)
	}
	if e.Verdict == VerdictChangesRequested {
		note("verdict changes_requested @"+ShortSHA(e.Head), LaneReasonChangesRequested)
	}
	if e.Checks.Known && len(e.Checks.Red) > 0 {
		var red []string
		for _, name := range e.Checks.Red {
			if !slices.Contains(e.ForkGuard, name) {
				red = append(red, name)
			}
		}
		if len(red) > 0 {
			note("red checks: "+strings.Join(red, ", "), laneReasonCheckFailing+red[0])
		}
	}
	if e.DCO == DCONo {
		note("DCO sign-off missing", LaneReasonDCOMissing)
	}
	if e.Conflict {
		note("merge conflict with the base branch", LaneReasonConflicting)
	}
	if len(fix) > 0 {
		return Route{Stage: fixing, Reasons: fix, LaneReason: laneReason, Summary: summary}
	}
	// A person's own PR with a requires_human verdict and nothing else
	// outstanding: the reviewer's question is the author's to answer. It
	// routes to 071 Needs author with the question, like a change request
	// (hivecommons/hive#15202). A hold, sentinel or lane rule still makes it
	// a decision below; a hive-authored or clanker PR has no human author
	// to ask, so it keeps the 085 route.
	if HumanAuthorOwesAnswer(e) {
		reasons := []string{"the reviewer asked the author a question @" + ShortSHA(e.Head)}
		if q := strings.TrimSpace(e.DecisionQuestion); q != "" {
			reasons = append(reasons, "reviewer question: "+q)
		} else {
			for _, summary := range e.HumanSummaries {
				if s := strings.TrimSpace(summary); s != "" {
					reasons = append(reasons, "reviewer summary: "+s)
				}
			}
		}
		return Route{Stage: fixing, Reasons: reasons, LaneReason: LaneReasonReviewerQuestion, Summary: summary, Head: e.Head, Verdict: e.Verdict}
	}
	// A fresh head waits for its verdict — unless the reviewer has already
	// answered requires_human for this very head, in which case the person
	// is owed a decision now, not after CI reports (hivecommons/hive#13115).
	if len(e.ForkGuard) == 0 && (e.Verdict == VerdictPending || (e.HeadFresh && e.Verdict != VerdictRequiresHuman)) {
		return Route{Stage: ReviewPending, Reasons: []string{"no reviewer verdict for this head yet"}, Summary: summary}
	}
	var decide []string
	if len(e.ForkGuard) > 0 {
		subject, verb, it := "check", "refuses", "it"
		if len(e.ForkGuard) > 1 {
			subject, verb, it = "checks", "refuse", "them"
		}
		decide = append(decide, subject+" `"+strings.Join(e.ForkGuard, "`, `")+"` "+verb+" pull requests from a fork, so neither the author nor the hive can turn "+it+" green: a maintainer must push this branch to the repository (or approve the run as the repository requires)")
	}
	approver := strings.TrimSpace(e.HumanApprover)
	switch e.Verdict {
	case VerdictUnavailable:
		decide = append(decide, "no reviewer agent is configured, so this head will never get a swarm verdict")
	case VerdictRequiresHuman:
		if approver != "" {
			// The person the reviewer deferred to has approved this very
			// head: the decision is made, not owed.
			break
		}
		decide = append(decide, "the reviewer swarm returned `requires_human` @"+ShortSHA(e.Head))
		if f := strings.TrimSpace(e.DecisionFinding); f != "" {
			decide = append(decide, "the review found: "+f)
		}
		if q := strings.TrimSpace(e.DecisionQuestion); q != "" {
			if decisionQuestionPlaceholderMention(q) {
				q += " _(the reviewer named no specific person; repository approvers/owners, this is yours)_"
			}
			decide = append(decide, "reviewer question: "+q)
		} else {
			for _, summary := range e.HumanSummaries {
				if s := strings.TrimSpace(summary); s != "" {
					decide = append(decide, "reviewer summary: "+s)
				}
			}
		}
	}
	if e.Sentinel {
		decide = append(decide, "a sentinel alert is open on this PR")
	}
	for _, h := range e.Holds {
		if h == "hold" && e.Hold.Clause() != "" {
			decide = append(decide, e.Hold.Clause())
			continue
		}
		decide = append(decide, "the `"+h+"` label is set")
	}
	manualMergeApproved := e.Cause == CauseDecision && e.ManualMergeRule && approver != "" && strings.TrimSpace(e.CauseReason) != "" && len(decide) == 0
	if e.Cause == CauseDecision && strings.TrimSpace(e.CauseReason) != "" && !manualMergeApproved {
		decide = append(decide, e.CauseReason)
	} else if e.Cause == CauseDecision && len(e.ForkGuard) == 0 && !manualMergeApproved {
		decide = append(decide, "the automerge lane will not merge this unattended")
	}
	if len(decide) > 0 {
		return Route{Stage: NeedsDecision, Reasons: decide, Summary: summary, Head: e.Head, SelfAuthored: e.SelfAuthored, Verdict: e.Verdict, HumanApprover: approver, Hold: e.Hold}
	}
	var wait []string
	switch {
	case !e.Checks.Known:
		wait = append(wait, "checks could not be read")
	case e.Checks.Pending > 0:
		wait = append(wait, fmt.Sprintf("%d check(s) still running", e.Checks.Pending))
	}
	if e.Soak > 0 && e.SoakElapsed < e.Soak {
		// A head the lane has NOT admitted (trust blocks it) is still
		// 060 while its soak runs; 095 pre-merge-soak is the lane's own
		// soak after admission (see PreMergeSoakSummary).
		wait = append(wait, fmt.Sprintf("soak %s remaining", (e.Soak-e.SoakElapsed).Round(time.Minute)))
	}
	if len(wait) > 0 {
		return Route{Stage: Reviewed, Reasons: wait, Summary: summary, HumanApprover: approver}
	}
	if manualMergeApproved {
		// A manual-merge lane rule the trusted merger has ruled on: the
		// hive still never merges the head itself, so the PR is ready for
		// the person's merge click — a 090 with one named step, not an
		// open-ended 085 (hivecommons/hive#14568, rationguard#401).
		route := Route{Stage: ReadyForHumanMerge, ManualMerge: true, Summary: summary, Head: e.Head, SelfAuthored: e.SelfAuthored, Verdict: e.Verdict, HumanApprover: approver,
			Reasons: []string{e.CauseReason + "; @" + approver + " approved this head, so the only step left is the merge"}}
		if lane := strings.TrimSpace(e.LaneReason); lane != "" {
			route.LaneReason = lane + HumanApprovedReasonSuffix + approver
		}
		return route
	}
	if e.Cause == CauseTrust && e.ManualMergeRule && strings.TrimSpace(e.CauseReason) != "" {
		// A manual-merge rule that owes no decision — the breaking-change
		// title or the needs-owner-merge label (hivecommons/hive#15442):
		// the head is green and reviewed, and the hive still never merges
		// it; the owner's merge click is the one step left.
		route := Route{Stage: ReadyForHumanMerge, ManualMerge: true, Summary: summary, Head: e.Head, SelfAuthored: e.SelfAuthored, Verdict: e.Verdict, HumanApprover: approver,
			Reasons: []string{e.CauseReason}, LaneReason: strings.TrimSpace(e.LaneReason)}
		if approver != "" && route.LaneReason != "" {
			route.LaneReason += HumanApprovedReasonSuffix + approver
		}
		return route
	}
	return Route{Stage: ReadyForHumanMerge, Summary: summary, HumanApprover: approver}
}

// decisionQuestionPlaceholderMentionRE matches the policy example's
// `@maintainer` / `@maintainers` placeholder copied verbatim into a decision
// question; GitHub notifies no one for it.
var decisionQuestionPlaceholderMentionRE = regexp.MustCompile(`(?i)(^|[^\w/])@maintainers?($|[^\w-])`)

// decisionQuestionPlaceholderMention reports whether a decision question
// addresses the `@maintainer` placeholder instead of a real login, so the
// 085 comment can say that nobody was pinged.
func decisionQuestionPlaceholderMention(q string) bool {
	return decisionQuestionPlaceholderMentionRE.MatchString(q)
}

// ShortSHA is the 7-char head rendering used in comments.
func ShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func summarize(e Evidence) string {
	var parts []string
	switch e.Verdict {
	case VerdictApproved:
		parts = append(parts, "verdict approved @"+ShortSHA(e.Head))
	case VerdictChangesRequested:
		parts = append(parts, "verdict changes_requested @"+ShortSHA(e.Head))
	case VerdictUnavailable:
		parts = append(parts, "no reviewer configured")
	case VerdictRequiresHuman:
		parts = append(parts, "verdict requires_human @"+ShortSHA(e.Head))
	default:
		parts = append(parts, "verdict pending @"+ShortSHA(e.Head))
	}
	if approver := strings.TrimSpace(e.HumanApprover); approver != "" {
		parts = append(parts, "approved by @"+approver)
	}
	if requester := strings.TrimPrefix(strings.TrimSpace(e.HumanChangeRequester), "@"); requester != "" {
		parts = append(parts, "changes requested by @"+requester)
	}
	switch {
	case !e.Checks.Known:
		parts = append(parts, "checks unknown")
	case len(e.Checks.Red) > 0:
		parts = append(parts, fmt.Sprintf("%d/%d checks green, red: %s", e.Checks.Green, e.Checks.Total, strings.Join(e.Checks.Red, ", ")))
	case e.Checks.Pending > 0:
		parts = append(parts, fmt.Sprintf("%d/%d checks green, %d pending", e.Checks.Green, e.Checks.Total, e.Checks.Pending))
	default:
		parts = append(parts, fmt.Sprintf("%d/%d checks green", e.Checks.Green, e.Checks.Total))
	}
	switch e.DCO {
	case DCOYes:
		parts = append(parts, "DCO yes")
	case DCONo:
		parts = append(parts, "DCO no")
	}
	if e.Sentinel {
		parts = append(parts, "sentinel alert")
	}
	if e.Conflict {
		parts = append(parts, "merge conflict")
	}
	if e.Soak > 0 {
		if e.SoakElapsed >= e.Soak {
			parts = append(parts, "soak "+e.Soak.Round(time.Minute).String()+" elapsed")
		} else {
			parts = append(parts, fmt.Sprintf("soak %s of %s", e.SoakElapsed.Round(time.Minute), e.Soak.Round(time.Minute)))
		}
	}
	if len(e.Holds) > 0 {
		parts = append(parts, "labels: "+strings.Join(e.Holds, ", "))
	}
	return strings.Join(parts, ", ")
}

// PreMergeSoakSummary is the one-line 095 explanation — "soaking: admitted,
// 12m of 30m elapsed" — for the stage comment, dashboard and logs. With no
// soak window configured it reads "soaking: admitted, no soak window".
func PreMergeSoakSummary(soak, elapsed time.Duration) string {
	if soak <= 0 {
		return "soaking: admitted, no soak window"
	}
	if elapsed > soak {
		elapsed = soak
	}
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("soaking: admitted, %s of %s elapsed", elapsed.Round(time.Minute), soak.Round(time.Minute))
}

// Entry-comment markers for the two human stages.
const (
	ReadyCommentMarker    = "<!-- hive:stage-ready-for-human-merge -->"
	DecisionCommentMarker = "<!-- hive:stage-needs-decision -->"
)

// ReadyComment renders the 090 entry comment:
//
//	Ready for a human merge: verdict approved @abc1234, 55/55 checks green,
//	DCO yes, soak 2h elapsed. Blocked only by `role/newcomer` (below
//	require_tiers).
//
// blockedBy is the prose for the trust cause ("" when unknown).
//
// An approved manual-merge route (r.ManualMerge) names the lane rule and
// the approver instead of a trust cause, and tells the person the one step
// left — the merge click — since no review or reply can make the hive
// merge CI workflow edits itself:
//
//	Ready for a human merge: verdict requires_human @abc1234, approved by
//	@octocat, 55/55 checks green, DCO yes. The PR edits CI workflow /
//	action files, which the lane never merges unattended; @octocat approved
//	this head, so the only step left is the merge.
func ReadyComment(r Route, blockedBy string) string {
	var b strings.Builder
	b.WriteString(ReadyCommentMarker)
	b.WriteString("\nReady for a human merge: ")
	b.WriteString(r.Summary)
	b.WriteString(".")
	if r.ManualMerge {
		for _, reason := range r.Reasons {
			if reason = strings.TrimSpace(reason); reason != "" {
				b.WriteString(" ")
				b.WriteString(strings.ToUpper(reason[:1]) + reason[1:])
				b.WriteString(".")
			}
		}
		b.WriteString("\n\n<sub>The hive never merges this lane's changes itself: click **Merge** (or `/lgtm` + `/approve` for tide) to land it.</sub>")
		return b.String()
	}
	if blockedBy = strings.TrimSpace(blockedBy); blockedBy != "" {
		b.WriteString(" Blocked only by ")
		b.WriteString(blockedBy)
		b.WriteString(".")
	}
	b.WriteString("\n\n<sub>The hive never approves on your behalf: `/lgtm` `/approve` (or merge) if you accept it.</sub>")
	return b.String()
}

// DecisionComment renders the 085 entry comment naming what is owed.
func DecisionComment(r Route) string {
	var b strings.Builder
	b.WriteString(DecisionCommentMarker)
	if r.ManualMerge {
		b.WriteString("\nNeeds a manual merge decision")
	} else {
		b.WriteString("\nNeeds a decision, not a merge")
	}
	if len(r.Reasons) == 1 {
		b.WriteString(": ")
		b.WriteString(r.Reasons[0])
		b.WriteString(".")
	} else {
		b.WriteString(":\n")
		for _, reason := range r.Reasons {
			b.WriteString("- ")
			b.WriteString(reason)
			b.WriteString("\n")
		}
	}
	if r.Summary != "" {
		b.WriteString("\n\nHead: ")
		b.WriteString(r.Summary)
		b.WriteString(".")
	}
	if actions := DecisionActions(r); actions != "" {
		b.WriteString("\n\n")
		b.WriteString(actions)
	}
	if len(r.Actions) > 0 {
		b.WriteString("\n\n**How to release it:**\n\n")
		for _, action := range r.Actions {
			b.WriteString("- " + action + "\n")
		}
	}
	b.WriteString("\n\n<sub>Clear the cause and the hive re-evaluates on its next pass; the stage moves to `" + ReadyForHumanMerge.Label() + "`, `" + PreMergeSoak.Label() + "` or the merge queue on its own. Approval does not bypass CI, hold labels or other merge gates.</sub>")
	return b.String()
}

// DecisionActions names the two things a maintainer can do with a
// hive-authored PR the swarm parked on `requires_human`: approve the head
// to merge it as-is, or request changes — by review or `/hive decision` —
// and the hive pushes the fix (hivecommons/hive#13041). Empty for every
// other 085 route: a hold label or sentinel alert has its own remedy, and
// the hive does not push to a PR it did not open.
func DecisionActions(r Route) string {
	if r.Stage != NeedsDecision || !r.SelfAuthored || r.Verdict != VerdictRequiresHuman {
		return ""
	}
	return "**What to do:** approve this head (`" + ShortSHA(r.Head) + "`) to merge as-is, or leave a *changes requested* review (or reply `/hive decision <what to change>`) and the hive will push the fix."
}

// HumanAuthorOwesAnswer reports whether a requires_human verdict on a PR a
// person pushed themselves routes to the author (071) rather than to a
// decision (085): the author is human and not this hive, no trusted merger
// has approved the head, and nothing else — sentinel, hold label, fork
// guard, a human-only lane rule — asks a maintainer for a decision.
func HumanAuthorOwesAnswer(e Evidence) bool {
	if e.SelfAuthored || e.AuthorKind != authorkind.Human || e.Verdict != VerdictRequiresHuman {
		return false
	}
	if strings.TrimSpace(e.HumanApprover) != "" || e.Sentinel || len(e.Holds) > 0 || len(e.ForkGuard) > 0 {
		return false
	}
	return e.Cause != CauseDecision || strings.TrimSpace(e.CauseReason) == "" || IsDecisionRequestedReason(e.CauseReason)
}

// IsDecisionRequestedReason matches the lane reason the reviewer's
// decision-request verdict is recorded under; it names no lane rule of
// its own, so it never turns a human author's question into a decision.
func IsDecisionRequestedReason(reason string) bool {
	return strings.EqualFold(strings.TrimSpace(reason), "contributor-lane:decision-requested")
}
