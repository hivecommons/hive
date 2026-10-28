package github

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
)

// AdvisoryWorkState reads an explicitly referenced issue/PR. The issues API
// identifies PRs; only those need a pulls lookup to distinguish merged from
// closed without merging. No search or remediation inference is performed.
func (c *Client) AdvisoryWorkState(ctx context.Context, owner, repo string, number int) (advisory.LinkedWork, error) {
	if c == nil || c.client == nil {
		return advisory.LinkedWork{}, ErrNoGitHubClient
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return advisory.LinkedWork{}, err
	}
	work := advisory.LinkedWork{Kind: "issue", State: strings.ToUpper(issue.GetState())}
	if issue.IsPullRequest() {
		fullRepo := owner + "/" + repo
		// The issue GET above is fresh (and ETag-cheap); the PR detail is only
		// needed to tell merged from closed-unmerged. A cached detail may be
		// reused only when it agrees with the issue's state — a cached OPEN
		// entry cannot answer for an issue GitHub now reports closed.
		pr, ok := c.cachedPRDetailAny(fullRepo, number)
		if ok && !strings.EqualFold(pr.GetState(), issue.GetState()) {
			ok = false
		}
		if !ok {
			var err error
			pr, _, err = c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:advisory_work"), owner, repo, number)
			if err != nil {
				return advisory.LinkedWork{}, err
			}
			c.storePRDetail(fullRepo, number, pr)
		}
		work.Kind, work.State = "pr", strings.ToUpper(pr.GetState())
		if pr.GetMerged() {
			work.State = "MERGED"
		}
	}
	work.CheckedAt = time.Now()
	return work, nil
}

// IssueTitleState reads an issue's title and lower-cased state with one issues
// GET. A number that names a pull request, open or closed, yields empty
// strings and isPR: the caller only wants an issue's title, and a PR must not
// be drawn in an issue's place.
func (c *Client) IssueTitleState(ctx context.Context, owner, repo string, number int) (title, state string, isPR bool, err error) {
	if c == nil || c.client == nil {
		return "", "", false, ErrNoGitHubClient
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", "", false, err
	}
	if issue.IsPullRequest() {
		return "", "", true, nil
	}
	return issue.GetTitle(), strings.ToLower(issue.GetState()), false, nil
}
