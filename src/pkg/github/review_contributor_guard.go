package github

import (
	"context"
	"fmt"
	"strings"
)

// The review swarm may READ any pull request, but it may only ADJUDICATE the
// hive's own (hivecommons/hive#9590). A contributor's PR gets a COMMENT — a
// review the author can weigh and ignore — and never an APPROVE or a
// REQUEST_CHANGES, which are repository verdicts that gate merge queues,
// satisfy branch-protection approval counts and read to the contributor as the
// project speaking.
//
// The guard lives HERE, in the relay, not in the reviewer's prompt: the prompt
// is advice, and `reviewEventToAPI` accepted all three verbs on any PR whoever
// wrote it. With review.all_authors on, one badly-worded kick was enough to put
// an approving review on an outside contributor's PR.

// contributorCommentOnlyFallbackBody is the review body used when an APPROVE
// on a contributor PR is downgraded and the request carried no body of its own.
// APPROVE is the one event GitHub accepts with an empty body; COMMENT is not,
// so the downgrade has to have something to say.
const contributorCommentOnlyFallbackBody = "Reviewed by the hive's review swarm and no blocking findings were raised.\n\n" +
	"_This is a comment, not an approval: the hive only approves pull requests it opened itself._"

// commentOnlyGuardActive reports whether the relay can tell this hive's own
// PRs from everyone else's. With no identity configured at all — no
// project.ai_author, no App bot login — every author is a stranger, and
// downgrading every review on that basis would silently disable approvals for
// a hive that simply has not told us who it is. Such a hive keeps the
// pre-guard behaviour; every hive the boot path configures (see
// SetHiveIdentity) gets the guard.
func (c *Client) commentOnlyGuardActive() bool {
	if c == nil {
		return false
	}
	return !c.getHiveIdentity().IsZero() || strings.TrimSpace(c.appBotLogin) != ""
}

// contributorReviewReason reports whether repo#number is a contributor PR —
// one this hive did not open — and why we say so.
//
// Unlike the self-authorization gate, "we could not tell" resolves TOWARDS the
// contributor: an unreadable or unattributable PR is not proof that the hive
// wrote it, and the failure this guard exists to prevent (approving someone
// else's work) is the one that must not happen on a flaky API call. The worst
// case of being wrong the other way is a comment where an approval was meant,
// which a maintainer can still read and act on.
func (c *Client) contributorReviewReason(ctx context.Context, repo string, number int) (bool, string) {
	if c == nil || c.client == nil {
		return true, "this hive has no GitHub client, so the pull request's author cannot be established"
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_request_watcher"), owner, repoName, number)
	if err != nil {
		return true, fmt.Sprintf("the pull request's author could not be read (%v)", err)
	}
	user := pr.GetUser()
	login := strings.TrimSpace(SafeGetLogin(user))
	if login == "" {
		return true, "the pull request has no identifiable author"
	}
	if c.isHumanAuthor(user) {
		return true, fmt.Sprintf("@%s is not one of this hive's accounts", login)
	}
	return false, ""
}

// enforceCommentOnlyForContributorPR downgrades an APPROVE or REQUEST_CHANGES
// on a contributor PR to a COMMENT, returning the event and audit state to use
// plus a note for the request's result file (empty when nothing changed). A
// COMMENT is returned untouched without costing an API call, which is the
// common case: it is what the review swarm posts by default.
func (c *Client) enforceCommentOnlyForContributorPR(ctx context.Context, req ReviewRequest, apiEvent, state string) (string, string, string) {
	if apiEvent == "" || apiEvent == "COMMENT" || !c.commentOnlyGuardActive() {
		return apiEvent, state, ""
	}
	contributor, why := c.contributorReviewReason(ctx, req.Repo, req.Number)
	if !contributor {
		return apiEvent, state, ""
	}
	note := fmt.Sprintf("%s downgraded to COMMENT on contributor pull request %s#%d: %s",
		apiEvent, req.Repo, req.Number, why)
	if c.logger != nil {
		c.logger.Warn("review-request watcher: contributor PR review downgraded to COMMENT",
			"agent", req.Agent, "repo", req.Repo, "number", req.Number,
			"requested_event", apiEvent, "reason", why)
	}
	commentEvent, commentState, _ := reviewEventToAPI("comment")
	return commentEvent, commentState, note
}

// contributorCommentBody supplies a body for a downgraded review that had
// none. Only APPROVE can reach here empty; shape validation already refuses an
// empty REQUEST_CHANGES or COMMENT.
func contributorCommentBody(body string) string {
	if strings.TrimSpace(body) != "" {
		return body
	}
	return contributorCommentOnlyFallbackBody
}
