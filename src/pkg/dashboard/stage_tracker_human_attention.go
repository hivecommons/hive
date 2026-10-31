package dashboard

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerHumanAttention is a finished-snapshot requirement on this exact
// item, not an available decision action or the kind of its claim holder.
type StageTrackerHumanAttention struct {
	Number  int    `json:"number"`
	Kind    string `json:"kind"`
	Head    string `json:"head,omitempty"`
	Reason  string `json:"reason"`
	AskedBy string `json:"asked_by,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

func stageTrackerHumanAttention(it StageTrackerItem) *StageTrackerHumanAttention {
	if !stageTrackerNeedsHuman(it) || it.Stage >= stagelabels.MergeQueue.Number() ||
		slices.Contains(it.Modifiers, modDraft) || slices.Contains(it.Modifiers, "merged") {
		return nil
	}
	// Mirrored issues carry their PR's observation on the link, never an
	// issue-wide requirement inherited from a parent RFC or a sibling PR.
	if it.Kind == "issue" && it.Mirrored {
		return nil
	}
	a := &StageTrackerHumanAttention{Number: it.Number, Kind: it.Kind, Head: it.Head}
	if d := it.DecisionNeeded; d != nil {
		a.Reason = strings.TrimSpace(nonEmpty(d.Question, d.Blocker))
		a.AskedBy = strings.TrimSpace(d.AskedBy)
	}
	if a.Reason == "" {
		a.Reason = strings.TrimSpace(it.HumanMergeReasonText)
	}
	if a.Reason == "" {
		a.Reason = stageTrackerDecisionWhy(it)
	}
	if actor := it.StageActor; actor != nil && actor.Kind == StageActorHuman {
		a.Owner = actor.Login
		if a.Reason == "" {
			switch stageByNumber(it.Stage) {
			case stagelabels.TriageNeeded, stagelabels.PossiblyExisting, stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor:
				a.Reason = strings.TrimSpace(actor.Why)
			}
		}
	}
	if a.Reason == "" {
		a.Reason = "Human decision required; reason unavailable"
	}
	return a
}

// Resolve only after operator/decision projections, current PR link hydration
// and actor resolution, so cleared decisions and new heads cannot retain a
// previous link's requirement. No traversal of issue/RFC ancestry is involved.
func stageTrackerResolveHumanAttention(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		it := &items[i]
		it.HumanAttention = stageTrackerHumanAttention(*it)
		if it.Kind == "pr" {
			prs[it.Repo+"#"+strconv.Itoa(it.Number)] = it
		}
	}
	for i := range items {
		it := &items[i]
		for j := range it.LinkedPRs {
			lp := &it.LinkedPRs[j]
			lp.HumanAttention = nil
			pr := prs[it.Repo+"#"+strconv.Itoa(lp.Number)]
			if pr == nil || pr.Head != lp.Head || lp.State == "closed" ||
				lp.SupersededBy > 0 || lp.AdoptedBy > 0 {
				continue
			}
			lp.HumanAttention = pr.HumanAttention
		}
	}
}
