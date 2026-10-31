package review

import "time"

// Reviewer slot ordering (hivecommons/hive#15823).
//
// The parallel review budget is spent in PR order, so the order IS the
// policy. Before this the order was the caller's queue rank with starved
// heads walked to the front; a PR whose verdict was the only thing between
// it and a merge waited behind PRs whose CI was still running — and a head
// reviewed while its CI is pending is the head most likely to be pushed
// again, wasting the slot. The planner now orders: merge-ready first (CI
// green, mergeable: the verdict unblocks a merge), starved heads second,
// the rest in the caller's order (its queue rank, age within class), and
// pending-CI heads last, so they take a slot only when one would otherwise
// go unused this cycle.

// Priority reasons, stamped onto DispatchKick.Priority and
// PullRequest.Priority.
const (
	ReviewPriorityMergeReady       = "merge-ready"
	ReviewPriorityStarved          = "starved"
	ReviewPriorityAge              = "age"
	ReviewPriorityCIPendingSpare   = "ci-pending-spare"
	ReviewPriorityPreparation      = "base-preparation"
	ReviewPriorityPreparationYield = "base-preparation-fairness"
)

// orderByReviewPriority returns prs in dispatch order, each stamped with its
// Priority. promoted are the starved heads promoteStarvedWaits moved up. The
// order within each band is the incoming one, so the caller's ranking still
// decides ties.
func orderByReviewPriority(prs []PullRequest, promoted []ReviewWaitRecord, org string, yieldPreparation ...bool) []PullRequest {
	if len(prs) == 0 {
		return prs
	}
	starved := make(map[string]bool, len(promoted))
	for _, w := range promoted {
		starved[reviewKey(fullRepoName(w.Repo, org), w.Number, w.HeadSHA, "")] = true
	}
	hasPreparation := false
	for _, pr := range prs {
		hasPreparation = hasPreparation || (pr.PreparationOwner && pr.MergeReady && !pr.CIPending)
	}
	const (
		bandAged = iota
		bandPreparation
		bandReady
		bandStarved
		bandAge
		bandPending
		bandCount
	)
	bands := [bandCount][]PullRequest{}
	for _, pr := range prs {
		preparing := pr.PreparationOwner && pr.MergeReady && !pr.CIPending
		isStarved := starved[reviewKey(fullRepoName(pr.Repo, org), pr.Number, pr.HeadSHA, "")]
		switch {
		case hasPreparation && len(yieldPreparation) > 0 && yieldPreparation[0] && isStarved && !preparing && !pr.CIPending:
			// Reuse durable wait promotion to bound preferential handoff:
			// a stream of preparation owners cannot starve aged others.
			pr.Priority = ReviewPriorityPreparationYield
			bands[bandAged] = append(bands[bandAged], pr)
		case preparing:
			pr.Priority = ReviewPriorityPreparation
			bands[bandPreparation] = append(bands[bandPreparation], pr)
		case pr.MergeReady:
			pr.Priority = ReviewPriorityMergeReady
			bands[bandReady] = append(bands[bandReady], pr)
		case pr.CIPending:
			pr.Priority = ReviewPriorityCIPendingSpare
			bands[bandPending] = append(bands[bandPending], pr)
		case isStarved:
			pr.Priority = ReviewPriorityStarved
			bands[bandStarved] = append(bands[bandStarved], pr)
		default:
			pr.Priority = ReviewPriorityAge
			bands[bandAge] = append(bands[bandAge], pr)
		}

	}
	out := make([]PullRequest, 0, len(prs))
	for _, b := range bands {
		out = append(out, b...)
	}
	return out
}

// Use the existing confirmed-dispatch history, not planned/undelivered kicks,
// to alternate preferential handoff with the existing aged-waiter promotion.
func lastReviewWasPreparation(recent []RecentReview) bool {
	var prepared, yielded time.Time
	for _, r := range recent {
		switch r.Priority {
		case ReviewPriorityPreparation:
			if r.Confirmed.After(prepared) {
				prepared = r.Confirmed
			}
		case ReviewPriorityPreparationYield:
			if r.Confirmed.After(yielded) {
				yielded = r.Confirmed
			}
		}
	}
	// Explicit aged service in the same confirmed batch also pays the yield;
	// unrelated deliveries never do, regardless of timestamp or slice order.
	return prepared.After(yielded)
}
