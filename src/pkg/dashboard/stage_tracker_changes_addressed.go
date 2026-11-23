package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// Changes addressed, re-approve (hivecommons/hive#19538).
//
// A person's CHANGES_REQUESTED review keeps GitHub's review decision at
// "changes requested" until that person reviews again. Once the hive has
// addressed every point, the review-feedback sweep re-requests their review
// (github.RerequestReview); a reviewer who requested changes and is again a
// requested reviewer is therefore waiting to re-review addressed work, not
// a decision nobody has framed. The row says so, and an owner can approve
// the head. A human review is never dismissed.

const (
	// changesAddressedWord is the short state the row, the next-move line
	// and the review signal read.
	changesAddressedWord = "changes addressed, re-approve"
)

// stageTrackerChangesAddressedBy returns the logins whose change request
// stands on pr while their review is requested again, in the order GitHub
// listed the change requests. It is nil unless every person who requested
// changes has been re-requested: one outstanding change request is still
// a change request.
func stageTrackerChangesAddressedBy(pr github.PullRequest) []string {
	if pr.Protection == nil || len(pr.Protection.ChangesRequestedBy) == 0 || len(pr.RequestedReviewers) == 0 {
		return nil
	}
	requested := make(map[string]bool, len(pr.RequestedReviewers))
	for _, login := range pr.RequestedReviewers {
		requested[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(login), "@"))] = true
	}
	var out []string
	for _, login := range pr.Protection.ChangesRequestedBy {
		login = strings.TrimPrefix(strings.TrimSpace(login), "@")
		if login == "" {
			continue
		}
		if !requested[strings.ToLower(login)] {
			return nil
		}
		out = append(out, login)
	}
	return out
}

// changesAddressedQuestion is the decision popover's question for a PR
// whose change requests are addressed and re-requested.
func changesAddressedQuestion(reviewers []string, sha string) string {
	who := make([]string, 0, len(reviewers))
	for _, login := range reviewers {
		who = append(who, "@"+login)
	}
	on := ""
	if sha != "" {
		on = " on " + sha
	}
	return "Changes addressed, re-approve: every point " + strings.Join(who, ", ") + " asked for has been addressed" + on + " and their review was re-requested. Approve this head to merge it, or wait for the reviewer?"
}
