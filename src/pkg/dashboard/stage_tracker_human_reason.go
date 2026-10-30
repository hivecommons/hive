package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Owner (hivecommons/hive#15773): "when i see 'pr in progress' on the
// chevron - i have no idea when it needs a human at that stage". The 👤
// marker on a lit chevron must always come with its reason: the chevron
// word names what the person is wanted for ("PR in progress · #15759
// needs a decision", "Ready for human merge · workflow files"), and the
// full question stays in the tooltip. stageTrackerHumanReason is the ONE
// place that reason is worded, from the fields the payload already
// carries; stageTrackerResolveHumanReasons stamps it on every item (and
// nested PR) whose next move is a person's, so the SPA never draws the
// marker alone.
//
// The same pass settles the covered-issue rule the owner raised on
// #15503: an issue riding a PR that waits on a decision does not inherit
// the PR's actionable chip — it carries decision_pending_on (the PR
// number) and a caption that says the PR is what waits — only the PR row
// offers "Let hive decide".

// stageTrackerHumanReasonMax bounds the chevron's reason: a few words,
// never a sentence (the tooltip has the sentence).
const stageTrackerHumanReasonMax = 40

// Chevron reasons shared by more than one place.
const (
	humanReasonNeedsDecision   = "needs a decision"
	humanReasonSentinel        = "security finding"
	humanReasonTriage          = "needs triage"
	humanReasonPriorArt        = "confirm prior art"
	humanReasonAdvisory        = "read & close"
	humanReasonHumanMerge      = "a maintainer merges"
	humanReasonRedundant       = "close as redundant?"
	humanReasonReviewerQuery   = "reviewer's question"
	humanReasonPermissions     = "hive not permitted"
	humanReasonEscalated       = "flagged needs-human"
	humanReasonSupersededClose = "superseded — close?"
	humanReasonWaitsOnPerson   = "waits on a person"
)

// stageTrackerHumanReason words why a person is the next mover on it, in
// at most stageTrackerHumanReasonMax characters; "" when the item's next
// move is not a person's. covering is the PR an issue rides (nil for a PR
// or an issue riding nothing in the snapshot).
func stageTrackerHumanReason(it StageTrackerItem, covering *StageTrackerItem) string {
	if it.StageActor == nil || it.StageActor.Kind != StageActorHuman {
		return ""
	}
	return stageTrackerHumanReasonClip(stageTrackerHumanReasonWords(it, covering))
}

func stageTrackerHumanReasonWords(it StageTrackerItem, covering *StageTrackerItem) string {
	// An issue riding its PR: the PR is what waits on the person.
	if covering != nil && it.Kind == "issue" {
		ref := "#" + strconv.Itoa(covering.Number)
		if why := stageTrackerHumanReasonWords(*covering, nil); why != "" {
			return ref + " " + stageTrackerHumanReasonForPR(why, covering)
		}
		return ref + " " + humanReasonWaitsOnPerson
	}
	if it.Superseded != nil && it.Superseded.CloserPR > 0 {
		return humanReasonSupersededClose
	}
	if it.SentinelAlert {
		return humanReasonSentinel
	}
	if adv := it.Advisory; adv != nil {
		return humanReasonAdvisory
	}
	if d := it.DecisionNeeded; d != nil {
		if d.RedundantOf > 0 {
			return humanReasonRedundant
		}
		switch d.AskedBy {
		case decisionAskedBySentinel:
			return humanReasonSentinel
		case decisionAskedByReviewer:
			return humanReasonReviewerQuery
		case decisionAskedByApp, decisionAskedByBlocker:
			return humanReasonPermissions
		case decisionAskedByEscalation:
			return humanReasonEscalated
		case decisionAskedByTriage:
			return humanReasonTriage
		}
	}
	switch it.Stage {
	case trackerNumberTriageNeeded:
		return humanReasonTriage
	case trackerNumberPossiblyExists:
		return humanReasonPriorArt
	case trackerNumberNeedsDecision, trackerNumberIssueDecision:
		if step, _ := stageTrackerUnauthorized(it.Reason); step != "" {
			return humanReasonPermissions
		}
		return humanReasonNeedsDecision
	case trackerNumberReadyForHuman:
		if _, word, _ := stageTrackerHumanMergeReason(it.Reason); word != "" {
			return word
		}
		return humanReasonHumanMerge
	case trackerNumberFixing:
		return stageTrackerHumanReasonAuthor(it, "pushes a fix")
	case trackerNumberNeedsAuthor, trackerNumberReturned:
		return stageTrackerHumanReasonAuthor(it, "must act")
	}
	if slices.Contains(it.Modifiers, modNeedsDecision) || slices.Contains(it.Modifiers, modNeedsHuman) {
		return humanReasonNeedsDecision
	}
	// The actor line's own words, clipped: never an empty reason.
	if why := strings.TrimSpace(it.StageActor.Why); why != "" {
		return why
	}
	return humanReasonWaitsOnPerson
}

// stageTrackerHumanReasonForPR turns a PR's own reason into the clause an
// issue's chevron reads after "#N": "needs a decision" stays, a merge-gate
// word becomes "needs a human merge", an author clause keeps its verb.
func stageTrackerHumanReasonForPR(why string, pr *StageTrackerItem) string {
	switch pr.Stage {
	case trackerNumberReadyForHuman:
		return "needs a human merge"
	case trackerNumberNeedsDecision, trackerNumberIssueDecision:
		if why == humanReasonSentinel || why == humanReasonPermissions || why == humanReasonRedundant {
			return humanReasonNeedsDecision + " · " + why
		}
		return humanReasonNeedsDecision
	}
	return why
}

func stageTrackerHumanReasonAuthor(it StageTrackerItem, verb string) string {
	if a := strings.TrimSpace(it.Author); a != "" {
		return "@" + a + " " + verb
	}
	return "the author " + verb
}

// stageTrackerHumanReasonClip trims to the budget on a word boundary.
func stageTrackerHumanReasonClip(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= stageTrackerHumanReasonMax {
		return s
	}
	runes := []rune(s)
	cut := stageTrackerHumanReasonMax - 1
	for i := cut; i > stageTrackerHumanReasonMax/2; i-- {
		if runes[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " ,;—-·") + "…"
}

// stageTrackerCoveredDecisionCaption is the covered issue's caption while
// its PR waits on a decision: the PR is what waits, not the issue.
func stageTrackerCoveredDecisionCaption(pr int) string {
	return "Covered by PR #" + strconv.Itoa(pr) + " — waiting for a decision on the PR"
}

// stageTrackerLeadLinkedPR is the linked PR an issue rides: the one drawn
// under this issue, else the first listed; nil without linked PRs.
func stageTrackerLeadLinkedPR(it *StageTrackerItem) *StageTrackerLinkedPR {
	if len(it.LinkedPRs) == 0 {
		return nil
	}
	for j := range it.LinkedPRs {
		if lp := &it.LinkedPRs[j]; lp.DrawnUnder == 0 || lp.DrawnUnder == it.Number {
			return lp
		}
	}
	return &it.LinkedPRs[0]
}

// stageTrackerIssueRidesPR reports an issue whose actor and caption are
// its covering PR's (the same test stageTrackerResolveActors applies).
func stageTrackerIssueRidesPR(it *StageTrackerItem) bool {
	return it.Kind == "issue" && !slices.Contains(it.Modifiers, modHold) &&
		(it.Mirrored || it.Stage == trackerNumberPRInProgress || it.Stage == trackerNumberCoveredByPR || it.Actor == "its PR")
}

// stageTrackerResolveHumanReasons runs after stageTrackerResolveActors:
// every item whose next mover is a person carries human_reason; nested
// PRs carry their PR's; an issue riding a PR parked on a decision carries
// decision_pending_on and the covered-decision caption.
func stageTrackerResolveHumanReasons(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		if items[i].Kind == "pr" {
			prs[items[i].Repo+"#"+strconv.Itoa(items[i].Number)] = &items[i]
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind == "pr" {
			it.HumanReason = stageTrackerHumanReason(*it, nil)
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		for j := range it.LinkedPRs {
			if pr := prs[it.Repo+"#"+strconv.Itoa(it.LinkedPRs[j].Number)]; pr != nil {
				it.LinkedPRs[j].HumanReason = pr.HumanReason
			}
		}
		var covering *StageTrackerItem
		if stageTrackerIssueRidesPR(it) {
			if lead := stageTrackerLeadLinkedPR(it); lead != nil {
				covering = prs[it.Repo+"#"+strconv.Itoa(lead.Number)]
			}
		}
		if covering != nil && stageTrackerPRWaitsOnDecision(covering) {
			it.DecisionPendingOn = covering.Number
			it.DecisionNeeded = nil
			it.Caption = stageTrackerCoveredDecisionCaption(covering.Number)
		}
		it.HumanReason = stageTrackerHumanReason(*it, covering)
	}
}

// stageTrackerPRWaitsOnDecision: the PR is parked at 085 on a person's
// decision. Other human stops (071 needs-author, 090 human merge) keep the
// issue's ordinary "Covered by PR #N — <pr caption>" narration.
func stageTrackerPRWaitsOnDecision(pr *StageTrackerItem) bool {
	return pr.Stage == trackerNumberNeedsDecision && pr.StageActor != nil && pr.StageActor.Kind == StageActorHuman
}
