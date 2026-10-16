package automerge

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Skip reasons reported by the review-verdict gate (#12374).
const (
	// autoMergeReasonReviewRequiresHuman: the review swarm's latest decisive
	// verdict on the PR asks for a person, and no person has released it.
	autoMergeReasonReviewRequiresHuman = "review-verdict-requires-human"
	// autoMergeReasonReviewRejected: the latest decisive verdict rejects the
	// PR, and no person has released it.
	autoMergeReasonReviewRejected = "review-verdict-reject"
	// autoMergeReasonReviewHoldEvidence: a verdict holds the PR but its
	// reviews or comments could not be listed to look for a person's
	// release. Fails CLOSED.
	autoMergeReasonReviewHoldEvidence = "review-verdict-hold-evidence"
)

// reviewHoldReleaseCommand is the reply a person posts to release a review
// verdict hold without submitting an approving review.
const reviewHoldReleaseCommand = "/hive approve"

// ReviewVerdictHold is the review swarm's hold on one pull request: the
// latest decisive verdict asked for a person (requires_human) or rejected it.
// HeadSHA is the head the verdict was recorded on, which may be older than
// the PR's current head: a fix push does not clear the hold.
type ReviewVerdictHold struct {
	Held       bool
	Verdict    string
	HeadSHA    string
	RecordedAt time.Time
}

// ReviewVerdictHoldFunc resolves the review verdict hold for repo#number.
// repo is owner/repo. It is consulted regardless of review.require_approval.
type ReviewVerdictHoldFunc func(repo string, number int) ReviewVerdictHold

// reviewVerdictGate refuses an App or trusted-author self-merge while the
// review swarm's verdict holds the PR for a person (#12374). The hold is not
// keyed to the current head: a requires_human verdict followed by a CI fix
// push still holds the new head until a fresh merge-eligible approve is
// recorded (the hook stops reporting it) or a person releases it on GitHub
// with an approving review or a `/hive approve` reply posted after the
// verdict.
//
// It returns ("", nil) when the merge may proceed. No hook or no hold costs
// no API call.
func (c *Engine) reviewVerdictGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) (string, error) {
	if c == nil || c.reviewVerdictHold == nil {
		return "", nil
	}
	number := pr.GetNumber()
	hold := c.reviewVerdictHold(owner+"/"+repo, number)
	if !hold.Held {
		return "", nil
	}
	reason := autoMergeReasonReviewRequiresHuman
	if strings.EqualFold(hold.Verdict, "reject") {
		reason = autoMergeReasonReviewRejected
	}
	released, by, err := c.personReleasedReviewHold(ctx, owner, repo, number, hold.RecordedAt)
	if err != nil {
		return autoMergeReasonReviewHoldEvidence, err
	}
	if released {
		c.info("automerge sweep: review verdict hold released by a person", "repo", displayRepo, "pr", number, "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "by", by)
		return "", nil
	}
	c.info("automerge sweep held PR for a person: review verdict", "repo", displayRepo, "pr", number, "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "current_head", pr.GetHead().GetSHA())
	return reason, nil
}

// personReleasedReviewHold reports whether a person approved the PR, or
// replied `/hive approve`, after the hold's verdict was recorded. A plain
// comment does not release: "do not merge this" is a comment too.
func (c *Engine) personReleasedReviewHold(ctx context.Context, owner, repo string, number int, since time.Time) (bool, string, error) {
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	reviewOpts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, reviewOpts)
		if err != nil {
			return false, "", err
		}
		for _, r := range reviews {
			if !strings.EqualFold(r.GetState(), "APPROVED") || !c.isPersonLogin(r.GetUser()) {
				continue
			}
			if r.GetSubmittedAt().Time.After(since) {
				return true, hgithub.SafeGetLogin(r.GetUser()), nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}
	commentOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	if !since.IsZero() {
		commentOpts.Since = gh.Ptr(since)
	}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, commentOpts)
		if err != nil {
			return false, "", err
		}
		for _, comment := range comments {
			if !c.isPersonLogin(comment.GetUser()) || !comment.GetCreatedAt().Time.After(since) {
				continue
			}
			if isReviewHoldReleaseCommand(comment.GetBody()) {
				return true, hgithub.SafeGetLogin(comment.GetUser()), nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, "", nil
		}
		commentOpts.Page = resp.NextPage
	}
}

func isReviewHoldReleaseCommand(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) >= 2 && fields[0]+" "+fields[1] == reviewHoldReleaseCommand {
			return true
		}
	}
	return false
}

// isPersonLogin reports whether u is a person, not the App or another bot.
func (c *Engine) isPersonLogin(u *gh.User) bool {
	login := strings.TrimSpace(hgithub.SafeGetLogin(u))
	if login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if c.transport != nil && strings.EqualFold(login, strings.TrimSpace(c.transport.AppBotLogin())) {
		return false
	}
	return true
}
