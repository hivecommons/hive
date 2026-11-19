package automerge

import (
	"context"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Issue-author credit on the sweep's merges (hivecommons/hive#14581): every
// lane builds its merge request through mergeOptions, which asks the
// transport's MergeCreditor for the Co-authored-by trailers naming the human
// authors of the issues the PR closes and stashes the decision so the
// pr_merged audit and the evidence merge event can name them. A transport
// without a creditor (test stubs, no API client) merges exactly as before.

type mergeCreditTransport interface {
	MergeCreditor() *hgithub.MergeCreditor
	NoteMergeCredit(repo string, number int, credit hgithub.MergeCredit)
}

// mergeOptions returns the commit message and options for merging pr's
// head at headSHA with method. The message is "" (GitHub's default) unless
// the repo credits issue authors and the PR has one to credit, or a squash
// needs the body rebuilt to keep the branch's Signed-off-by trailers
// (hivecommons/hive#18203). A non-empty refused is the skip reason when the
// squash would land with no sign-off at all; the caller must not merge.
func (c *Engine) mergeOptions(ctx context.Context, owner, repo string, pr *gh.PullRequest, headSHA, method string) (message string, opts *gh.PullRequestOptions, refused string) {
	opts = &gh.PullRequestOptions{SHA: headSHA, MergeMethod: method}
	mt, ok := c.transport.(mergeCreditTransport)
	if !ok || pr == nil {
		return "", opts, ""
	}
	creditor := mt.MergeCreditor()
	if creditor.Enabled(owner + "/" + repo) {
		credit := creditor.Prepare(ctx, owner, repo, pr, method)
		mt.NoteMergeCredit(owner+"/"+repo, pr.GetNumber(), credit)
		message = credit.CommitMessage
	}
	message, err := creditor.SquashSignoff(ctx, owner, repo, pr.GetNumber(), method, message)
	if err != nil {
		// An unsigned branch needs its author to sign off; this is an
		// expected merge refusal, not a runtime failure.
		c.info("automerge refused squash without Signed-off-by", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "error", err)
		return "", opts, autoMergeReasonSquashUnsigned
	}
	return message, opts, ""
}

// autoMergeReasonSquashUnsigned is the skip reason when no branch commit
// carries a Signed-off-by, so the squash would land a commit that fails DCO.
const autoMergeReasonSquashUnsigned = "squash-unsigned"
