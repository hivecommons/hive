package dashboard

import (
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The "Needs human" popover (hivecommons/hive#14994, owner request
// 2026-10-28): hovering the
// *Needs human decision* pill or the needs-human chip must say WHAT the
// person is being asked to decide, who asked, and which of the row's
// actions advance it. The tracker payload carries that as
// `decision_needed` on every row parked on a person: the reviewer's
// actual question (the `decision_question` the verdict relay recorded in
// review-verdicts.json for the PR's current head — the same text the 085
// stage comment prints after "reviewer question:"), the blocking reason in
// plain words, and the applicable actions under the SAME ids the row
// context menu uses, so the popover's buttons and the menu share one code
// path in the SPA.

// StageTrackerDecision is the decision a person owes on a tracker row.
type StageTrackerDecision struct {
	// Question is what the person must decide, in the asker's own words
	// when the hive has them (the reviewer's decision question), else a
	// sentence derived from the park reason.
	Question string `json:"question"`
	// AskedBy names who parked the item: "reviewer swarm", "sentinel",
	// "lane rule", "GitHub App permissions", "triage gate", "hive agent",
	// "escalation ledger".
	AskedBy string `json:"asked_by"`
	// Since is when the question was asked: the verdict's recorded time
	// for a reviewer question, else when the item entered its stage.
	Since *time.Time `json:"since,omitempty"`
	// Blocker says in plain words what stops the hive from proceeding on
	// its own (and, where it applies, what a human approval cannot do);
	// BlockerReason is the lane token behind it
	// ("contributor-lane:changes-requested", "sentinel-alert", …).
	Blocker       string `json:"blocker,omitempty"`
	BlockerReason string `json:"blocker_reason,omitempty"`
	// Detail is the technical background the popover folds behind one
	// collapsed "Details" (how to trust the author, the rails, DCO): never
	// in the first read (hivecommons/hive#15394).
	Detail string `json:"detail,omitempty"`
	// RedundantOf is the PR the reviewer named as already carrying this
	// PR's change (`<!-- hive-redundant-of: #N -->`), when the question is
	// "close this as a no-op?" (hivecommons/hive#15499). The popover shows
	// it as a chip and leads with "Close as superseded".
	RedundantOf int `json:"redundant_of,omitempty"`
	// Options are the actions that can advance the item, first the one the
	// owner asked to lead ("Let hive decide"), under the row context menu's
	// action ids. A disabled option says why; the SPA further greys out
	// what the signed-in viewer's role does not allow.
	Options []StageTrackerDecisionOption `json:"options"`
}

// StageTrackerDecisionOption is one action the popover offers.
type StageTrackerDecisionOption struct {
	Action           string `json:"action"`
	Label            string `json:"label"`
	Enabled          bool   `json:"enabled"`
	ReasonIfDisabled string `json:"reason_if_disabled,omitempty"`
	// Consequence says, in one clause, what happens when the action is
	// taken ("merges #317 into main now via hive's identity, attributed to
	// you"); the ⋯ menu and the popover print it under the label
	// (hivecommons/hive#15078).
	Consequence string `json:"consequence,omitempty"`
	// URL is set on link options (open:app-settings): the SPA opens it
	// instead of posting an action (hivecommons/hive#15099).
	URL string `json:"url,omitempty"`
	// Lead marks a destructive option (decide:close-*) the row may show as
	// its inline lead button. The SPA never leads a row with a close
	// unless the server says the close is the answer — a reviewer naming
	// the PR that supersedes this one (RedundantOf). Without it a human's
	// PR waiting on its author led with "Close as superseded" just because
	// the non-destructive options already sat in the Decide… submenu.
	Lead bool `json:"lead,omitempty"`
}

// Action ids shared with the SPA's row context menu
// (STAGE_TRACKER_ACTION_LABELS in index.html).
const (
	decisionActionDelegate        = "decide:delegate"
	decisionActionApprove         = "decide:approve"
	decisionActionDirection       = "decide:direction"
	decisionActionCloseSuperseded = "decide:close-superseded"
	decisionActionCloseWontfix    = "decide:close-wontfix"
	decisionActionMergeNow        = "decide:merge-now"
	decisionActionQueueMerge      = "decide:queue-merge"
	decisionActionReauthor        = "decide:re-author"
	decisionActionAcceptTriage    = "accept-triage"
	decisionActionHold            = "hold"
	decisionActionRelease         = "release"
	decisionActionNeedsHuman      = "needs-human"
	decisionActionRetry           = "retry"
)

// Who asked, in the plain words the popover prints (owner, 2026-10-28:
// "exceedingly clear, easy to read, and concise" — no hive jargon).
const (
	decisionAskedByReviewer   = "a reviewer"
	decisionAskedBySentinel   = "the security check"
	decisionAskedByLane       = "the hive"
	decisionAskedByApp        = "GitHub permissions"
	decisionAskedByTriage     = "triage"
	decisionAskedByHiveAgent  = "the hive"
	decisionAskedByEscalation = "the needs-human label"
)

// Blocker reason tokens the tracker coins when the lane left none. The
// token is for machines (filters, tests); the popover prints Blocker.
const (
	decisionBlockerSentinel = "sentinel-alert"
	decisionBlockerHold     = "hold"
	decisionBlockerHoldHive = "hold:hive"
)

const (
	decisionWhyHeld           = "On hold — release the hold first"
	decisionWhyNoPermission   = "Only a person with the permission can do this"
	decisionWhyApproveNoGrant = "Approving does not grant the missing permission"
	decisionHeldBlockerPlain  = "On hold — the hive does nothing until the hold is released."
)

// stageTrackerVerdictLookup returns a lookup over review-verdicts.json for
// the newest aggregate recorded for repo#number at head, mirroring the
// contributor lane's own match (cmd/hive contributorLaneVerdictFromArtifact)
// so the popover quotes the question the lane acted on. nil artifact → a
// lookup that never matches.
func stageTrackerVerdictLookup(artifact *review.Artifact) func(repo string, number int, head string) (review.Aggregate, bool) {
	return func(repo string, number int, head string) (review.Aggregate, bool) {
		if artifact == nil || number == 0 || strings.TrimSpace(head) == "" {
			return review.Aggregate{}, false
		}
		var match review.Aggregate
		var ok bool
		for _, item := range artifact.Items {
			item = item.Normalize()
			if item.Number != number || !strings.EqualFold(strings.TrimSpace(item.Repo), strings.TrimSpace(repo)) || !review.SameHeadSHA(item.HeadSHA, head) {
				continue
			}
			if !ok || item.RecordedAt.After(match.RecordedAt) {
				match, ok = item, true
			}
		}
		return match, ok
	}
}

// stageTrackerDecisionVerdictLookup is stageTrackerVerdictLookup with the
// fallback a parked row needs (hivecommons/hive#15499): when the current
// head has no verdict yet — the hive moved the branch onto a new base and
// the re-review is in flight — but the PR is parked at 085 on a reviewer's
// decision, the question that parked it is the newest decision-requested
// aggregate on any head of the PR. Without it the popover printed "a
// reviewer handed this PR to a person without a specific question" while
// the review on the previous head asked a specific one.
func stageTrackerDecisionVerdictLookup(artifact *review.Artifact) func(repo string, number int, head string, parkedOnDecision bool) (review.Aggregate, bool) {
	atHead := stageTrackerVerdictLookup(artifact)
	return func(repo string, number int, head string, parkedOnDecision bool) (review.Aggregate, bool) {
		if a, ok := atHead(repo, number, head); ok {
			return a, true
		}
		if !parkedOnDecision || artifact == nil || number == 0 {
			return review.Aggregate{}, false
		}
		var match review.Aggregate
		var ok bool
		for _, item := range artifact.Items {
			item = item.Normalize()
			if item.Number != number || !strings.EqualFold(strings.TrimSpace(item.Repo), strings.TrimSpace(repo)) || !item.DecisionRequested {
				continue
			}
			if !ok || item.RecordedAt.After(match.RecordedAt) {
				match, ok = item, true
			}
		}
		return match, ok
	}
}

// stageTrackerDecisionNeeded derives the decision a row asks of a person,
// or nil when nothing on the row waits on one. verdict is the review
// aggregate for the PR's current head when the artifact has one;
// sentinelLabel is the configured sentinel alert label.
func stageTrackerDecisionNeeded(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string) *StageTrackerDecision {
	return stageTrackerDecisionNeededAt(it, verdict, sentinelLabel, time.Now())
}

// stageTrackerDecisionNeededAt is stageTrackerDecisionNeeded with the clock
// the tracker build uses, so spans in questions match the row's pill.
func stageTrackerDecisionNeededAt(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string, now time.Time) *StageTrackerDecision {
	return stageTrackerDecisionNeededWith(it, verdict, sentinelLabel, now, blockerOptionsContext{})
}

// stageTrackerDecisionNeededWith is stageTrackerDecisionNeededAt with the
// server context a blocked row's options need (the App settings link).
func stageTrackerDecisionNeededWith(it StageTrackerItem, verdict *review.Aggregate, sentinelLabel string, now time.Time, bctx blockerOptionsContext) *StageTrackerDecision {
	held := slices.Contains(it.Modifiers, modHold)
	switch {
	case it.Kind == "pr" && it.MergeTrain != nil && it.MergeTrain.State == MergeGateStateRebaseExhausted:
		return decisionForMergeTrain(it, held)
	case it.Kind == "pr" && it.MergeGate != nil && mergeGateClaimConflictOf(it.MergeGate.State) > 0:
		return decisionForClaimConflict(it, held)
	case it.Kind == "issue" && it.Blocker != nil && !it.Mirrored && (slices.Contains(it.Modifiers, modNeedsHuman) || it.Stage == stagelabels.IssueNeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		// A structured blocker outranks the generic park wording: the
		// person is not being asked to decide, the hive is saying what it
		// cannot do (hivecommons/hive#15099).
		return decisionForBlocker(it, held, bctx)
	case it.Kind == "pr" && (it.Stage == stagelabels.NeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		return decisionForPR(it, verdict, sentinelLabel, held)
	case it.Kind == "pr" && it.Stage == stagelabels.ReadyForHumanMerge.Number():
		return decisionForHumanMerge(it, held)
	case it.Kind == "issue" && it.Mirrored:
		// The issue rides its covering PR; the PR's own row (nested under
		// the issue) carries the decision.
		return nil
	case it.Kind == "issue" && (it.Stage == stagelabels.IssueNeedsDecision.Number() || slices.Contains(it.Modifiers, modNeedsDecision)):
		return decisionForIssue(it, held)
	case it.Kind == "issue" && (it.Stage == stagelabels.TriageNeeded.Number() || slices.Contains(it.Modifiers, modNeedsTriage)):
		return decisionForTriage(it, held)
	case slices.Contains(it.Modifiers, modNeedsHuman):
		return decisionForNeedsHuman(it, held)
	case slices.Contains(it.Modifiers, modUnauthorized):
		return decisionForUnauthorized(it, held)
	case it.Kind == "pr" && it.Adoption != nil && it.Adoption.Action == decisionActionAdopt:
		return decisionForAdoption(it, held, now)
	case it.Kind == "pr" && it.Adoption != nil && it.Adoption.QueuedBy != "":
		// Adopt now already pressed: nothing is asked until the sweep lands.
		return nil
	case it.Kind == "pr" && it.Takeover != nil && it.Takeover.GraceElapsed && it.Adoption != nil && it.Adoption.BlockedBy != stagelabels.AdoptBlockedAuthorMaintainer:
		// The nudge grace ran out and the engine has not adopted (ask is
		// the case above; never stays the policy's word): the wait is a
		// person's to end (Adopt now / Reroute) — hivecommons/hive#15038.
		return decisionForWaitingOnContributor(it, held, now)
	case it.Kind == "pr" && it.AuthorKind == authorKindHuman && slices.Contains(stageTrackerNeedsHumanAuthorStages, it.Stage):
		if stageTrackerPushPending(it) {
			// The author pushed and CI / the re-review for the new head
			// still run: nothing to decide until the hive has looked
			// (hivecommons/hive#16021).
			return nil
		}
		return decisionForWaitingOnAuthor(it, held)
	case stageTrackerNeedsHuman(it) || (held && stageTrackerNeedsHuman(stageTrackerWithoutHold(it))):
		// Flagged for a person by a stage/kind pairing none of the cases
		// above knows (an issue wearing a PR stage label by hand, a PR at
		// a triage stage): still never a pill without an action.
		return decisionForParked(it, held)
	}
	return nil
}

// stageTrackerWithoutHold is the item as it reads with the hold pill off,
// so a held row still derives the decision its underlying state asks for.
func stageTrackerWithoutHold(it StageTrackerItem) StageTrackerItem {
	it.Modifiers = slices.DeleteFunc(slices.Clone(it.Modifiers), func(m string) bool { return m == modHold })
	return it
}
