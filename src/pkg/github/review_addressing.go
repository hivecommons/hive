package github

import "time"

// ReviewAddressed reports whether a human review submitted at reviewAt has
// been answered by agent-visible activity on the PR: either a non-merge commit
// on the head branch, or an agent reply, after the review timestamp. Merge
// commits alone are ignored so held-PR base-sync/CI repair does not hide a
// still-unanswered CHANGES_REQUESTED review.
func ReviewAddressed(reviewAt time.Time, commits []PRCommit, replies []PRComment) bool {
	if reviewAt.IsZero() {
		return false
	}
	for _, c := range commits {
		if !c.AuthoredAt.After(reviewAt) {
			continue
		}
		if c.ParentCount > 1 {
			continue
		}
		return true
	}
	for _, r := range replies {
		if r.CreatedAt.After(reviewAt) {
			return true
		}
	}
	return false
}

// HumanReviewAddressed applies ReviewAddressed to the latest trusted human
// opinionated review stamped on a PR's protection facts.
func HumanReviewAddressed(pr PullRequest) bool {
	if pr.Protection == nil {
		return false
	}
	if pr.Protection.LatestHumanReviewAddressed {
		return true
	}
	return ReviewAddressed(pr.Protection.LatestHumanReviewSubmittedAt, pr.ReviewAddressingCommits, pr.ReviewAddressingReplies)
}
