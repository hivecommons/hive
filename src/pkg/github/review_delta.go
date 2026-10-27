package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

// ReviewDeltaDiff fetches the compare between a PR's previously reviewed head
// and its current head for the review planner's delta re-review
// (review.delta_rereview). Both are immutable SHAs, so the answer cannot
// describe a newer push. One page of files is read: GitHub caps a compare's
// file list at githubCompareFileLimit per page, and a delta that large is not
// a candidate for the trivial skip anyway, so a full page is reported as
// unknown rather than paged. Any failure is also unknown, never an error: the
// planner then sends the delta instruction without a file list.
func (c *Client) ReviewDeltaDiff(ctx context.Context, repo, previousHead, head string) review.DeltaDiff {
	out := review.DeltaDiff{PreviousHead: strings.TrimSpace(previousHead)}
	head = strings.TrimSpace(head)
	if c == nil || c.client == nil || out.PreviousHead == "" || head == "" || review.SameHeadSHA(out.PreviousHead, head) {
		return out
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	comparison, resp, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:review_delta"), owner, name, out.PreviousHead, head, &gh.ListOptions{PerPage: githubCompareFileLimit})
	if err != nil || comparison == nil {
		return out
	}
	if (resp != nil && resp.NextPage != 0) || len(comparison.Files) >= githubCompareFileLimit {
		return out
	}
	out.Known = true
	for _, f := range comparison.Files {
		out.Files = append(out.Files, review.DeltaFile{Path: f.GetFilename(), Additions: f.GetAdditions(), Deletions: f.GetDeletions()})
	}
	return out
}
