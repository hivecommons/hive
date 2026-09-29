package github

import (
	"context"
	"strings"
)

// Contributor-PR review guard (hivecommons/hive#9608).
//
// With review.all_authors on, the review swarm reviews PRs this hive did not
// open. On those PRs the hive may say what it found, but it must never cast a
// formal vote: an APPROVE from the App bot can satisfy branch protection for
// a stranger's change, and a REQUEST_CHANGES blocks a person's PR on a bot's
// say-so. The relay therefore submits every review of a contributor PR as a
// COMMENT, whatever event the agent asked for and whatever the dispatch
// settings are. The check lives here, in the relay, rather than in the
// dispatcher or the prompt, because an agent can drop a request file for any
// PR number, dispatched or not.
//
// Downgrade was chosen over refusal. The agent did its job: the findings are
// real and the author is owed them, and the structured verdict still has to
// be recorded or the PR reads as never reviewed and is dispatched again from
// scratch. Refusing would lose both. The downgraded body carries a line
// naming the verdict the reviewer intended, so the author and maintainers
// still see it, only without GitHub treating it as a vote.

// Review event verbs as the GitHub REST API spells them.
const (
	reviewAPIEventApprove        = "APPROVE"
	reviewAPIEventRequestChanges = "REQUEST_CHANGES"
	reviewAPIEventComment        = "COMMENT"
)

// Audit/result states, the same strings reviewEventToAPI records.
const (
	reviewStateApproved  = "approved"
	reviewStateCommented = "commented"
)

// reviewRESTCallerContributorGuard labels the one PR GET the guard makes, so
// the REST budget view attributes it.
const reviewRESTCallerContributorGuard = "hive:review_contributor_guard"

// isFormalReviewVerdict reports whether a GitHub review event is a vote
// (APPROVE or REQUEST_CHANGES) rather than a plain COMMENT.
func isFormalReviewVerdict(apiEvent string) bool {
	return apiEvent == reviewAPIEventApprove || apiEvent == reviewAPIEventRequestChanges
}

// contributorSafeReviewEvent maps a request's event to the event the relay may
// submit given the PR's authorship. On a hive-authored PR it is exactly
// reviewEventToAPI. On any other PR a formal verdict becomes COMMENT and
// intendedState names what was asked for ("approved" / "changes_requested"),
// so callers can say so; intendedState is "" when nothing was changed.
// ok=false for an event reviewEventToAPI does not know.
func contributorSafeReviewEvent(event string, hiveAuthored bool) (apiEvent, state, intendedState string, ok bool) {
	apiEvent, state, ok = reviewEventToAPI(event)
	if !ok || hiveAuthored || !isFormalReviewVerdict(apiEvent) {
		return apiEvent, state, "", ok
	}
	return reviewAPIEventComment, reviewStateCommented, state, true
}

// reviewTargetHiveAuthored reports whether the PR a review request names was
// opened by this hive (its App bot or project.ai_author) or by another bot
// account, which is the set the review swarm reviews without
// review.all_authors (review.isAgentAuthored) and on which a formal verdict
// stays allowed. Anything else, a person's PR, is a contributor PR.
//
// It fails closed: a PR that cannot be read, or whose author login is absent,
// counts as a contributor PR, so a GitHub blip downgrades a vote to a comment
// rather than letting one through. The review is never lost either way. The
// second return is an operator-facing reason for a non-hive answer, for the
// result file and log only; it is never posted to the forge.
func (c *Client) reviewTargetHiveAuthored(ctx context.Context, req ReviewRequest) (bool, string) {
	owner, repoName := c.splitRepo(req.Repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, reviewRESTCallerContributorGuard), owner, repoName, req.Number)
	if err != nil {
		return false, "PR author could not be read (" + err.Error() + "); treated as a contributor PR"
	}
	user := pr.GetUser()
	if c.isKnownNonHumanAuthor(user) {
		return true, ""
	}
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return false, "PR author is unknown; treated as a contributor PR"
	}
	return false, "PR was opened by " + login + ", not by this hive"
}

// contributorVerdictNote prepends the line that tells the reader which formal
// verdict the reviewer meant, since the review itself now carries none. It is
// hive-authored text with no mentions, so it needs no sanitizing.
func contributorVerdictNote(intendedState, body string) string {
	verdict := "approve"
	if intendedState != reviewStateApproved {
		verdict = "request changes"
	}
	note := "_Reviewer verdict: " + verdict + ". This PR was not opened by this hive, so the hive leaves comment-only reviews on it and never approves or requests changes; a maintainer decides._"
	body = strings.TrimSpace(body)
	if body == "" {
		return note
	}
	return note + "\n\n" + body
}
