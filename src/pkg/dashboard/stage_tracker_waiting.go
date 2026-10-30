package dashboard

import (
	"slices"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Waiting-on-you aggregation (hivecommons/hive#15638): the stage tracker
// payload names every open item that is parked on the signed-in operator
// (or any maintainer) so the SPA can wear a second sidebar badge, a topbar
// pill and fire a toast. Before this the operator learned that a dozen
// PRs had sat at 085 needs-decision for 3–21 hours only by reading each
// row. The aggregation reuses the per-item fields the tracker already
// computes (Stage, Modifiers, DecisionNeeded, Adoption, Takeover, Decided,
// Advisory) and invents no new state.

// Wait kinds carried in StageTrackerWaitingItem.Kind and
// StageTrackerItem.WaitKind.
const (
	// waitKindNeedsDecision: 085 PR needs-decision / 045 issue
	// needs-decision, or the needs-decision modifier.
	waitKindNeedsDecision = "needs-decision"
	// waitKindReadyForHumanMerge: 090, a trusted human must press merge.
	waitKindReadyForHumanMerge = "ready-for-human-merge"
	// waitKindReturnedOffline: 071 / 072 with the contributor offline past
	// the grace, so the operator is the fallback (adopt / reroute).
	waitKindReturnedOffline = "returned-to-contributor-offline"
	// waitKindApproveNeeded: needs-human or unauthorized — the hive's App
	// lacks the permission or a lane rule wants a human approval.
	waitKindApproveNeeded = "approve-needed"
	// waitKindOther: any other stage only a person can advance (015
	// triage-needed).
	waitKindOther = "other-human-wait"
)

// waitWhoOperator is the Who value for a wait that falls to the signed-in
// operator / maintainers rather than a named contributor.
const waitWhoOperator = "operator"

// StageTrackerWaitingOnHuman is the payload header's waiting_on_human block.
type StageTrackerWaitingOnHuman struct {
	// Count is len(Items): the number the badges show.
	Count int `json:"count"`
	// Items lists the waits, longest waiting first.
	Items []StageTrackerWaitingItem `json:"items"`
}

// StageTrackerWaitingItem is one item parked on the operator.
type StageTrackerWaitingItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Type is pr or issue.
	Type string `json:"type"`
	// Kind is one of the waitKind* constants.
	Kind string `json:"kind"`
	// Since is when the wait began: the decision's asked time when one is
	// recorded, else when the item entered its stage.
	Since *time.Time `json:"since,omitempty"`
	// Who is the login the wait is attributed to: "operator" for the
	// signed-in operator / maintainers, else the contributor whose
	// offline grace ran out (the operator is their fallback).
	Who string `json:"who"`
}

// stageTrackerReturnedStages are the stages where the row waits on its
// author; the operator only inherits the wait once the author is offline
// past the grace.
var stageTrackerReturnedStages = []int{stagelabels.NeedsAuthor.Number(), stagelabels.ReturnedToContributor.Number()}

// stageTrackerWaitKind classifies the human wait on it, "" when the item
// waits on nobody the operator is answerable for. Hold, advisories and
// already-decided rows never count: hold is the operator's own choice,
// an advisory asks for nothing, and a decided row is the hive's to act
// on next.
func stageTrackerWaitKind(it StageTrackerItem) (kind, who string) {
	if stageTrackerOnHold(it) || it.Advisory != nil || it.Decided != nil {
		return "", ""
	}
	switch {
	case it.Stage == stagelabels.NeedsDecision.Number(), it.Stage == stagelabels.IssueNeedsDecision.Number(), slices.Contains(it.Modifiers, modNeedsDecision):
		return waitKindNeedsDecision, waitWhoOperator
	case it.Stage == stagelabels.ReadyForHumanMerge.Number():
		return waitKindReadyForHumanMerge, waitWhoOperator
	case slices.Contains(it.Modifiers, modNeedsHuman), slices.Contains(it.Modifiers, modUnauthorized):
		return waitKindApproveNeeded, waitWhoOperator
	case slices.Contains(stageTrackerReturnedStages, it.Stage):
		if !stageTrackerContributorGraceElapsed(it) {
			return "", ""
		}
		who := it.Author
		if it.Adoption != nil && it.Adoption.Author != "" {
			who = it.Adoption.Author
		}
		if who == "" {
			who = waitWhoOperator
		}
		return waitKindReturnedOffline, who
	case it.Stage == stagelabels.TriageNeeded.Number():
		return waitKindOther, waitWhoOperator
	}
	return "", ""
}

// stageTrackerContributorGraceElapsed reports whether a row returned to
// its contributor has outlived their grace, so the operator is now the
// one the hive waits on (adopt / reroute / nudge by hand).
func stageTrackerContributorGraceElapsed(it StageTrackerItem) bool {
	if it.Takeover != nil && (it.Takeover.GraceElapsed || it.Takeover.AdoptNow) {
		return true
	}
	if it.Adoption != nil && it.Adoption.Due {
		return true
	}
	return it.Presence != nil && it.Presence.Next == presenceNextAdopt
}

// stageTrackerWaitSince is when the wait on it began.
func stageTrackerWaitSince(it StageTrackerItem) *time.Time {
	if it.DecisionNeeded != nil && it.DecisionNeeded.Since != nil {
		return it.DecisionNeeded.Since
	}
	if it.Presence != nil && it.Presence.Since != nil {
		return it.Presence.Since
	}
	return it.StageSince
}

// stageTrackerWaitingOnHuman flags every item parked on the operator
// (WaitingOnYou / WaitKind) and returns the header block, longest wait
// first. Nested rows (an issue's covering PR drawn under it) are skipped
// so one decision is counted once.
func stageTrackerWaitingOnHuman(items []StageTrackerItem) StageTrackerWaitingOnHuman {
	out := StageTrackerWaitingOnHuman{Items: []StageTrackerWaitingItem{}}
	for i := range items {
		it := &items[i]
		it.WaitingOnYou, it.WaitKind = false, ""
		if it.NestedUnder != 0 {
			continue
		}
		kind, who := stageTrackerWaitKind(*it)
		if kind == "" {
			continue
		}
		it.WaitingOnYou, it.WaitKind = true, kind
		out.Items = append(out.Items, StageTrackerWaitingItem{Repo: it.Repo, Number: it.Number, Type: it.Kind, Kind: kind, Since: stageTrackerWaitSince(*it), Who: who})
	}
	sort.SliceStable(out.Items, func(a, b int) bool {
		sa, sb := out.Items[a].Since, out.Items[b].Since
		switch {
		case sa == nil && sb == nil:
			return false
		case sa == nil:
			return false
		case sb == nil:
			return true
		}
		return sa.Before(*sb)
	})
	out.Count = len(out.Items)
	return out
}
