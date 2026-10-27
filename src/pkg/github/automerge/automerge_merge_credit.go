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
// the repo credits issue authors and the PR has one to credit.
func (c *Engine) mergeOptions(ctx context.Context, owner, repo string, pr *gh.PullRequest, headSHA, method string) (string, *gh.PullRequestOptions) {
	opts := &gh.PullRequestOptions{SHA: headSHA, MergeMethod: method}
	mt, ok := c.transport.(mergeCreditTransport)
	if !ok || pr == nil {
		return "", opts
	}
	creditor := mt.MergeCreditor()
	if !creditor.Enabled(owner + "/" + repo) {
		return "", opts
	}
	credit := creditor.Prepare(ctx, owner, repo, pr, method)
	mt.NoteMergeCredit(owner+"/"+repo, pr.GetNumber(), credit)
	return credit.CommitMessage, opts
}
