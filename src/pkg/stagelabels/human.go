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
	// Cause is why the lane handed off; CauseTrust is the only one that can
	// end at 090.
	Cause HumanCause
	// CauseReason is the lane's reason string, rendered when Cause is a
	// decision the comment has to name.
	CauseReason string
	// DecisionQuestion is the maintainer-facing question from the reviewer
	// verdict/comment, when the swarm explicitly deferred a product call.
	DecisionQuestion string
	// HumanSummaries are reviewer summaries for requires_human verdicts that
	// did not carry a structured decision question.
	HumanSummaries []string
	// SelfAuthored selects 070 fixing for the "author must act" outcome;
	// otherwise AuthorKind picks 072 (clanker) or 071 (human / unknown).
	SelfAuthored bool
	AuthorKind   authorkind.Kind
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
	// Reasons explains the stage: what the author must fix (072/070), what
	// a person must decide (085), what is still running (060). Empty for 090.
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
}

// Lane-vocabulary reasons RouteHuman emits for the fixing stages.
const (
	LaneReasonChangesRequested       = "reviewer-verdict:changes_requested"
	LaneReasonDCOMissing             = "dco-missing"
	LaneReasonHiveSignoffAttribution = "hive-signoff-attribution"
	LaneReasonConflicting            = "conflicting"
	laneReasonCheckFailing           = "check-failing:"
	// LaneReasonAdoptionStalledPrefix opens the 085 reason for an adopted
	// PR (074) whose head sat past the stage dwell with nothing the hive
	// could push; the suffix is the lane reason it was stuck on
	// ("adoption-stalled:reviewer-verdict:changes_requested").
	LaneReasonAdoptionStalledPrefix = "adoption-stalled:"
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
	if e.Verdict == VerdictChangesRequested {
		note("verdict changes_requested @"+ShortSHA(e.Head), LaneReasonChangesRequested)
	}
	if e.Checks.Known && len(e.Checks.Red) > 0 {
		note("red checks: "+strings.Join(e.Checks.Red, ", "), laneReasonCheckFailing+e.Checks.Red[0])
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
	if e.Verdict == VerdictPending || e.HeadFresh {
		return Route{Stage: ReviewPending, Reasons: []string{"no reviewer verdict for this head yet"}, Summary: summary}
	}
	var decide []string
	switch e.Verdict {
	case VerdictUnavailable:
		decide = append(decide, "no reviewer agent is configured, so this head will never get a swarm verdict")
	case VerdictRequiresHuman:
		decide = append(decide, "the reviewer swarm returned `requires_human` @"+ShortSHA(e.Head))
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
		decide = append(decide, "the `"+h+"` label is set")
	}
	if e.Cause == CauseDecision && strings.TrimSpace(e.CauseReason) != "" {
		decide = append(decide, e.CauseReason)
	} else if e.Cause == CauseDecision {
		decide = append(decide, "the automerge lane will not merge this unattended")
	}
	if len(decide) > 0 {
		return Route{Stage: NeedsDecision, Reasons: decide, Summary: summary, Head: e.Head, SelfAuthored: e.SelfAuthored, Verdict: e.Verdict}
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
		return Route{Stage: Reviewed, Reasons: wait, Summary: summary}
	}
	return Route{Stage: ReadyForHumanMerge, Summary: summary}
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
func ReadyComment(r Route, blockedBy string) string {
	var b strings.Builder
	b.WriteString(ReadyCommentMarker)
	b.WriteString("\nReady for a human merge: ")
	b.WriteString(r.Summary)
	b.WriteString(".")
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
	b.WriteString("\nNeeds a decision, not a merge")
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
	b.WriteString("\n\n<sub>Clear the cause and the hive re-evaluates on its next pass; the stage moves to `" + ReadyForHumanMerge.Label() + "`, `" + PreMergeSoak.Label() + "` or the merge queue on its own.</sub>")
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
