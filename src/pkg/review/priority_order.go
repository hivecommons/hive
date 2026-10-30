package review

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
	ReviewPriorityMergeReady     = "merge-ready"
	ReviewPriorityStarved        = "starved"
	ReviewPriorityAge            = "age"
	ReviewPriorityCIPendingSpare = "ci-pending-spare"
)

// orderByReviewPriority returns prs in dispatch order, each stamped with its
// Priority. promoted are the starved heads promoteStarvedWaits moved up. The
// order within each band is the incoming one, so the caller's ranking still
// decides ties.
func orderByReviewPriority(prs []PullRequest, promoted []ReviewWaitRecord, org string) []PullRequest {
	if len(prs) == 0 {
		return prs
	}
	starved := make(map[string]bool, len(promoted))
	for _, w := range promoted {
		starved[reviewKey(fullRepoName(w.Repo, org), w.Number, w.HeadSHA, "")] = true
	}
	bands := [4][]PullRequest{}
	for _, pr := range prs {
		switch {
		case pr.MergeReady:
			pr.Priority = ReviewPriorityMergeReady
			bands[0] = append(bands[0], pr)
		case pr.CIPending:
			pr.Priority = ReviewPriorityCIPendingSpare
			bands[3] = append(bands[3], pr)
		case starved[reviewKey(fullRepoName(pr.Repo, org), pr.Number, pr.HeadSHA, "")]:
			pr.Priority = ReviewPriorityStarved
			bands[1] = append(bands[1], pr)
		default:
			pr.Priority = ReviewPriorityAge
			bands[2] = append(bands[2], pr)
		}
	}
	out := make([]PullRequest, 0, len(prs))
	for _, b := range bands {
		out = append(out, b...)
	}
	return out
}
