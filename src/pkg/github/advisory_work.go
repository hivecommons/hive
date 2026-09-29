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
		pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:advisory_work"), owner, repo, number)
		if err != nil {
			return advisory.LinkedWork{}, err
		}
		work.Kind, work.State = "pr", strings.ToUpper(pr.GetState())
		if pr.GetMerged() {
			work.State = "MERGED"
		}
	}
	work.CheckedAt = time.Now()
	return work, nil
}
