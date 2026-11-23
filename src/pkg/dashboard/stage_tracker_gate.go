package dashboard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// The human gate's one source of truth (hivecommons/hive#18769).
//
// A tracker row used to derive its "Needs human · review" pill from the
// cached reviewer verdict for the head (review_activity.state ==
// human_decision) while the verbs on that pill acted only on a PR the hive
// had actually parked on a person. A verdict never changes for a head, so
// once a person approved it the pill stayed and every verb answered
// "nothing to act on" (hivecommons/hive#18667). stageTrackerHumanGateFor is the
// single predicate now: it names the reasons a person is owed something
// right now, the verbs that would act, and — when nothing is owed — what
// the item is actually waiting on. The payload carries it as `human_gate`; the
// SPA's pill and its action precondition both read the same rule
// (tracker-gate-truth.js mirrors it for rows the page synthesizes, and
// testdata/gate_states.json pins the two implementations to one table).

// Gate reason keys, in the order they stand on the pill.
const (
	gateReasonReview   = "review"
	gateReasonDecision = "decision"
	gateReasonSentinel = "sentinel"
	gateReasonHold     = "hold"
)

// Gate verbs that are not decision actions (the decide:* ids live in
// stage_tracker_decision.go).
const (
	gateActionRequestChanges  = "gate:request-changes"
	gateActionDecide          = "gate:decide"
	gateActionConfirmSentinel = "gate:confirm-sentinel"
)

// What an item with no pending decision waits on.
const (
	gateWaitConflict      = "conflict"
	gateWaitAuthor        = "author"
	gateWaitFixing        = "fixing"
	gateWaitChecksFailing = "checks-failing"
	gateWaitChecksStuck   = "checks-stuck"
	gateWaitChecksMissing = "checks-missing"
	gateWaitCIRunning     = "ci-running"
	gateWaitReview        = "review-pending"
	gateWaitMergeQueue    = "merge-queue"
)

// How an answered sign-off ask was answered (human_gate.answered_via). The
// SPA's STAGE_TRACKER_ANSWERED_VIA_* mirror them: only a GitHub Approve
// review reads "signed off"; a dashboard approval names its login; the
// owner-delegate policy reads "answered by policy".
const (
	gateAnsweredViaReview    = "review"
	gateAnsweredViaDashboard = "dashboard"
	gateAnsweredViaPolicy    = "policy"
)

// stageTrackerHumanGateDelegateAllMin mirrors the SPA's
// STAGE_TRACKER_DELEGATE_ALL_MIN: an issue row with this many open PRs
// delegates all of them.
const stageTrackerHumanGateDelegateAllMin = 2

// stageTrackerHumanGateTerminal mirrors the SPA's STAGE_TRACKER_TERMINAL: a row
// at or past it is merged or closed.
const stageTrackerHumanGateTerminal = 110

// stageTrackerHumanGateShortSHA is how many characters of a head the waiting
// text prints.
const stageTrackerHumanGateShortSHA = 7

// stageTrackerHumanGateOrder is the canonical reason order.
var stageTrackerHumanGateOrder = []string{gateReasonReview, gateReasonDecision, gateReasonSentinel, gateReasonHold}

// stageTrackerHumanGateVerbs maps a reason to its verbs (the SPA's
// STAGE_TRACKER_GATE_VERBS).
var stageTrackerHumanGateVerbs = map[string][]string{
	gateReasonReview:   {decisionActionApprove, gateActionRequestChanges, decisionActionDelegate},
	gateReasonDecision: {gateActionDecide, decisionActionApprove, decisionActionDelegate},
	gateReasonSentinel: {gateActionConfirmSentinel, decisionActionDelegate},
	gateReasonHold:     {decisionActionRelease, decisionActionDelegate},
}

// stageTrackerHumanGatePROnly are the verbs that need a head to act on.
var stageTrackerHumanGatePROnly = []string{decisionActionApprove, gateActionRequestChanges}

// stageTrackerHumanGateAuthorDCO are the reasons that make needs_human the
// author's move rather than a decision (hivecommons/hive#17789).
var stageTrackerHumanGateAuthorDCO = []string{"dco-missing", "hive-signoff-attribution"}

// StageTrackerHumanGate is the live human-gate state of one tracker row.
type StageTrackerHumanGate struct {
	// Reasons a person is owed something right now, in pill order; empty
	// when nothing is.
	Reasons []string `json:"reasons"`
	// Actions are the verbs that would act on the item now. A verb absent
	// here answers "nothing to act on" and is never drawn.
	Actions []string `json:"actions"`
	// Answered is true when the reviewer asked for a person's sign-off on
	// this head and the hive no longer parks the PR on it; AnsweredBy is
	// the person who answered when one is known. AnsweredVia is how: a
	// GitHub Approve review (review), a dashboard approval (dashboard) or
	// the owner-delegate policy (policy); empty when the row does not say.
	Answered    bool   `json:"answered,omitempty"`
	AnsweredBy  string `json:"answered_by,omitempty"`
	AnsweredVia string `json:"answered_via,omitempty"`
	// WaitingOn names what the item actually waits on when no decision is
	// pending.
	WaitingOn *StageTrackerHumanGateWait `json:"waiting_on,omitempty"`
}

// StageTrackerHumanGateWait is the real blocker of a row nobody owes a decision on.
type StageTrackerHumanGateWait struct {
	// Kind is one of the gateWait* constants.
	Kind string `json:"kind"`
	// Text is the sentence the row and the stale-click toast print.
	Text string `json:"text"`
	// Checks names the required checks behind a checks-* kind.
	Checks []string `json:"checks,omitempty"`
}

// stageTrackerHumanGateInert reports rows that never carry a gate: closed,
// superseded, adopted, merged or draft.
func stageTrackerHumanGateInert(it StageTrackerItem) bool {
	if it.Superseded != nil || it.SupersededBy > 0 || it.AdoptedBy > 0 || it.Stage >= stageTrackerHumanGateTerminal {
		return true
	}
	return slices.Contains(it.Modifiers, "merged") || slices.Contains(it.Modifiers, modDraft)
}

// stageTrackerHumanGatePRParked mirrors the SPA's stageTrackerPRParked: the PR
// is parked on a person (045 / 085, the needs-decision pill, superseded, or
// ready for a human merge).
func stageTrackerHumanGatePRParked(stage int, modifiers []string, superseded, manualMerge bool, humanMergeReason string) bool {
	return superseded || slices.Contains(modifiers, modNeedsDecision) ||
		stage == trackerNumberIssueDecision || stage == trackerNumberNeedsDecision ||
		manualMerge || strings.TrimSpace(humanMergeReason) != ""
}

// stageTrackerHumanGateOpenLinkedPR reports a nested PR that is still in play.
func stageTrackerHumanGateOpenLinkedPR(lp StageTrackerLinkedPR) bool {
	return lp.State != "closed" && lp.SupersededBy == 0 && lp.AdoptedBy == 0 &&
		lp.Stage < stageTrackerHumanGateTerminal && !slices.Contains(lp.Modifiers, "merged")
}

// stageTrackerHumanGateDecisionTarget mirrors the SPA's
// stageTrackerDecisionTarget: whether a decide:* action on the row has an
// item to act on. A PR decides itself when parked; an issue decides its one
// parked nested PR, else itself when parked at 045.
func stageTrackerHumanGateDecisionTarget(it StageTrackerItem) bool {
	if it.Kind == "pr" {
		return stageTrackerHumanGatePRParked(it.Stage, it.Modifiers, it.Superseded != nil, it.ManualMerge, it.HumanMergeReason)
	}
	parked := 0
	for _, lp := range it.LinkedPRs {
		if stageTrackerHumanGatePRParked(lp.Stage, lp.Modifiers, false, lp.ManualMerge, lp.HumanMergeReason) && stageTrackerHumanGateOpenLinkedPR(lp) {
			parked++
		}
	}
	if parked > 0 {
		return parked == 1
	}
	if it.Mirrored {
		return false
	}
	return slices.Contains(it.Modifiers, modNeedsDecision) || it.Stage == trackerNumberIssueDecision
}

// stageTrackerHumanGateAccepts is the action precondition: whether verb would
// act on the row. It is the rule the SPA's runRowAction applies before it
// posts anything, so a verb this refuses is never offered.
func stageTrackerHumanGateAccepts(it StageTrackerItem, verb string) bool {
	switch {
	case verb == decisionActionDelegate:
		if stageTrackerHumanGateDecisionTarget(it) {
			return true
		}
		open := 0
		for _, lp := range it.LinkedPRs {
			if stageTrackerHumanGateOpenLinkedPR(lp) {
				open++
			}
		}
		return it.Kind != "pr" && open >= stageTrackerHumanGateDelegateAllMin
	case strings.HasPrefix(verb, "decide:"):
		return stageTrackerHumanGateDecisionTarget(it)
	}
	return true
}

// stageTrackerHumanGateOnHead reports an activity recorded for the row's head.
func stageTrackerHumanGateOnHead(a *StageTrackerActivity, head string) bool {
	return a != nil && (a.Head == "" || head == "" || strings.EqualFold(a.Head, head))
}

// stageTrackerHumanGateReasons derives the reasons a person is owed on the row.
func stageTrackerHumanGateReasons(it StageTrackerItem) []string {
	if stageTrackerHumanGateInert(it) {
		return nil
	}
	pr := it.Kind == "pr"
	target := stageTrackerHumanGateDecisionTarget(it)
	var out []string
	// A requires_human verdict is a live ask only while the hive parks the
	// PR on it. Off the park the person has answered (or the head is not
	// there yet): the verdict for the head stays as it was recorded.
	review := pr && stageTrackerHumanGateOnHead(it.ReviewActivity, it.Head) && it.ReviewActivity.State == "human_decision" && target
	if review {
		out = append(out, gateReasonReview)
	}
	decisionStage := it.Stage == trackerNumberNeedsDecision || (!pr && it.Stage == trackerNumberIssueDecision)
	labelled := decisionStage || slices.Contains(it.Modifiers, modNeedsDecision) || slices.Contains(it.Modifiers, modNeedsHuman)
	// needs_human alone (triage needed, a human author's turn, ready for
	// human merge) is a decision only where a decision can be recorded.
	bare := stageTrackerNeedsHuman(it) && !slices.Contains(stageTrackerHumanGateAuthorDCO, strings.TrimSpace(it.Reason)) && target
	blocked := it.Blocker != nil && strings.TrimSpace(it.Blocker.Short) != ""
	if (labelled || bare) && strings.TrimSpace(it.UnauthorizedStep) == "" && !blocked && !review {
		out = append(out, gateReasonDecision)
	}
	if it.SentinelAlert && !it.SentinelNotice {
		out = append(out, gateReasonSentinel)
	}
	if it.Hold != nil || slices.Contains(it.Modifiers, modHold) {
		out = append(out, gateReasonHold)
	}
	return out
}

// stageTrackerHumanGateActions is the union of the reasons' verbs, in reason
// order, de-duplicated, keeping only what the action precondition accepts.
func stageTrackerHumanGateActions(it StageTrackerItem, reasons []string) []string {
	out := []string{}
	for _, key := range stageTrackerHumanGateOrder {
		if !slices.Contains(reasons, key) {
			continue
		}
		for _, verb := range stageTrackerHumanGateVerbs[key] {
			if slices.Contains(out, verb) || (it.Kind != "pr" && slices.Contains(stageTrackerHumanGatePROnly, verb)) || !stageTrackerHumanGateAccepts(it, verb) {
				continue
			}
			out = append(out, verb)
		}
	}
	return out
}

// stageTrackerHumanGateFor is the gate of one row: nil for a row that never
// carries one.
func stageTrackerHumanGateFor(it StageTrackerItem, now time.Time) *StageTrackerHumanGate {
	if (it.Kind != "pr" && it.Kind != "issue") || stageTrackerHumanGateInert(it) {
		return nil
	}
	reasons := stageTrackerHumanGateReasons(it)
	g := &StageTrackerHumanGate{Reasons: append([]string{}, reasons...), Actions: stageTrackerHumanGateActions(it, reasons)}
	if it.Kind != "pr" {
		return g
	}
	if stageTrackerHumanGateOnHead(it.ReviewActivity, it.Head) && it.ReviewActivity.State == "human_decision" && !slices.Contains(reasons, gateReasonReview) {
		g.Answered = true
		g.AnsweredBy, g.AnsweredVia = stageTrackerHumanGateAnswerer(it)
	}
	if len(reasons) == 0 {
		g.WaitingOn = stageTrackerHumanGateWaitingOn(it, now)
	}
	return g
}

// stageTrackerHumanGateAnswerer names who answered a sign-off ask on the
// row's head and how. The `:human-approved@login` suffix (a GitHub Approve
// review) wins; otherwise the newest applied operator action on this head:
// a dashboard approve / queue-merge / merge-now credits its actor, and an
// approving let-hive-decide is the owner-delegate policy
// (delegatedecision.PolicyName), which has no person to credit. Both are
// empty when nothing on the row says.
func stageTrackerHumanGateAnswerer(it StageTrackerItem) (by, via string) {
	if approver := strings.TrimPrefix(strings.TrimSpace(it.HumanApprover), "@"); approver != "" {
		return approver, gateAnsweredViaReview
	}
	a := it.LastOperatorAction
	if a == nil || a.Outcome != operatorActionOutcomeApplied {
		return "", ""
	}
	if head := strings.TrimSpace(a.HeadSHA); head != "" && it.Head != "" && !strings.EqualFold(head, it.Head) {
		return "", ""
	}
	switch a.Action {
	case operatorActionApprove, operatorActionQueueMerge, operatorActionMergeNow:
		return strings.TrimPrefix(strings.TrimSpace(a.Actor), "@"), gateAnsweredViaDashboard
	case operatorActionLetHiveDecide:
		if a.Verdict == string(delegatedecision.VerdictApprove) {
			return "", gateAnsweredViaPolicy
		}
	}
	return "", ""
}

// stageTrackerHumanGateWaitingOn names what a PR nobody owes a decision on is
// waiting for, the first that applies: a conflict, its author, required
// checks (red, stuck, unreported, running), the review of this head, the
// merge queue.
func stageTrackerHumanGateWaitingOn(it StageTrackerItem, now time.Time) *StageTrackerHumanGateWait {
	if slices.Contains(it.Modifiers, modNeedsRebase) || (it.MergeTrain != nil && it.MergeTrain.State == MergeGateStateRebaseExhausted) {
		base := strings.TrimSpace(it.BaseRef)
		if base == "" {
			base = "its base branch"
		}
		return &StageTrackerHumanGateWait{Kind: gateWaitConflict, Text: "blocked by a merge conflict with " + base}
	}
	switch it.Stage {
	case trackerNumberNeedsAuthor, trackerNumberReturned:
		who := strings.TrimPrefix(strings.TrimSpace(it.Author), "@")
		if who == "" {
			return &StageTrackerHumanGateWait{Kind: gateWaitAuthor, Text: "the author's turn"}
		}
		return &StageTrackerHumanGateWait{Kind: gateWaitAuthor, Text: "the author's turn (@" + who + ")"}
	case trackerNumberFixing:
		return &StageTrackerHumanGateWait{Kind: gateWaitFixing, Text: "the hive fixing this head"}
	}
	if ci := it.CIActivity; stageTrackerHumanGateOnHead(ci, it.Head) {
		tally := ""
		if ci.ChecksTotal > 0 {
			tally = fmt.Sprintf(" (%d/%d)", ci.ChecksPassed, ci.ChecksTotal)
		}
		switch {
		case len(ci.ChecksFailing) > 0 && ci.State == "failing":
			return &StageTrackerHumanGateWait{Kind: gateWaitChecksFailing, Checks: ci.ChecksFailing, Text: "required checks failing: " + strings.Join(ci.ChecksFailing, ", ")}
		case len(ci.ChecksStuck) > 0:
			text := "required checks stuck: " + strings.Join(ci.ChecksStuck, ", ")
			if ci.StuckSince != nil && now.After(*ci.StuckSince) {
				text += " (in progress " + stageTrackerHumanGateSpan(now.Sub(*ci.StuckSince)) + ", far longer than a normal run)"
			}
			if len(ci.ChecksPending) > len(ci.ChecksStuck) {
				text += " · CI running" + tally
			}
			return &StageTrackerHumanGateWait{Kind: gateWaitChecksStuck, Checks: ci.ChecksStuck, Text: text}
		case ci.State == "missing" && len(ci.ChecksPending) > 0:
			return &StageTrackerHumanGateWait{Kind: gateWaitChecksMissing, Checks: ci.ChecksPending, Text: "required checks not reported: " + strings.Join(ci.ChecksPending, ", ")}
		case ci.State == "running" || ci.State == "queued":
			return &StageTrackerHumanGateWait{Kind: gateWaitCIRunning, Checks: ci.ChecksPending, Text: "CI running" + tally}
		}
	}
	if rv := it.ReviewActivity; stageTrackerHumanGateOnHead(rv, it.Head) {
		switch rv.State {
		case "approved", "human_decision":
		case "changes_requested":
			return &StageTrackerHumanGateWait{Kind: gateWaitAuthor, Text: "the author's turn: review findings on this head"}
		default:
			head := strings.TrimSpace(it.Head)
			if len(head) > stageTrackerHumanGateShortSHA {
				head = head[:stageTrackerHumanGateShortSHA]
			}
			if head == "" {
				return &StageTrackerHumanGateWait{Kind: gateWaitReview, Text: "the review of the current head"}
			}
			return &StageTrackerHumanGateWait{Kind: gateWaitReview, Text: "the review re-run on head " + head}
		}
	}
	if it.Stage >= trackerNumberAwaitingMerge || it.MergeGate != nil || it.MergeTrain != nil {
		text := "the merge queue"
		if mg := it.MergeGate; mg != nil && strings.TrimSpace(mg.Reason) != "" {
			text += " (" + strings.TrimSpace(mg.Reason) + ")"
		}
		return &StageTrackerHumanGateWait{Kind: gateWaitMergeQueue, Text: text}
	}
	return nil
}

// stageTrackerHumanGateSpan prints a wait as whole hours or minutes.
func stageTrackerHumanGateSpan(d time.Duration) string {
	if d >= time.Hour {
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m"
}

// stageTrackerResolveHumanGates sets every row's gate and mirrors a listed PR's
// gate onto the links that draw it. It runs after the decision, actor and
// human-attention projections so it reads the finished row.
func stageTrackerResolveHumanGates(items []StageTrackerItem, now time.Time) {
	stageTrackerResolveMidFlight(items)
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		it := &items[i]
		it.HumanGate = stageTrackerHumanGateFor(*it, now)
		if it.Kind == "pr" {
			prs[it.Repo+"#"+strconv.Itoa(it.Number)] = it
		}
	}
	for i := range items {
		it := &items[i]
		for j := range it.LinkedPRs {
			lp := &it.LinkedPRs[j]
			lp.HumanGate = nil
			if pr := prs[it.Repo+"#"+strconv.Itoa(lp.Number)]; pr != nil && pr.Head == lp.Head && stageTrackerHumanGateOpenLinkedPR(*lp) {
				lp.HumanGate = pr.HumanGate
			}
		}
	}
}
