package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// closedSweepRESTCaller tags the stage tracker's closed-item sweep in the
// REST accounting (/api/gh-rate-limits).
const closedSweepRESTCaller = "dashboard:closed-sweep"

// closedSincePerPage is GitHub's maximum page size for the issues list.
const closedSincePerPage = 100

// ListClosedSince lists the issues and pull requests of repo that are
// closed and were updated at or after since, newest first, as closed
// WebhookItemSnapshots — one GET per page, at most maxPages pages (a
// non-positive maxPages reads one). GitHub's issues list returns pull
// requests too, so a single call covers both kinds. The read goes through
// the paced REST transport and is refused, like the webhook re-read, while
// the core budget is critical or the rate-limit caution window is engaged.
// Each listed item is also observed into the attached state cache
// (forgestate) so the governor's work source drops it as well.
//
// The stage tracker's backstop sweep uses this to evict rows whose close
// the webhook path missed (hivecommons/hive#14159, #14064).
func (c *Client) ListClosedSince(ctx context.Context, repo string, since time.Time, maxPages int) ([]WebhookItemSnapshot, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	if mode, _ := c.APIBudgetMode(); mode == APIBudgetCritical {
		return nil, ErrAPIBudgetCritical
	}
	if RateLimitCautionEngaged() {
		return nil, ErrRateLimitCaution
	}
	if maxPages <= 0 {
		maxPages = 1
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, closedSweepRESTCaller)
	now := time.Now()
	var out []WebhookItemSnapshot
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("state", "closed")
		q.Set("sort", "updated")
		q.Set("direction", "desc")
		q.Set("per_page", strconv.Itoa(closedSincePerPage))
		q.Set("page", strconv.Itoa(page))
		if !since.IsZero() {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		req, err := c.client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/issues?%s", owner, repoName, q.Encode()), nil)
		if err != nil {
			return out, err
		}
		var issues []*gh.Issue
		resp, err := c.client.Do(ctx, req, &issues)
		if err != nil {
			return out, fmt.Errorf("listing %s closed since %s: %w", repo, since.UTC().Format(time.RFC3339), err)
		}
		for _, issue := range issues {
			if issue == nil || issue.GetNumber() <= 0 {
				continue
			}
			snap := WebhookItemSnapshot{Repo: repo, Number: issue.GetNumber(), IsPR: issue.IsPullRequest(),
				Title: issue.GetTitle(), Author: safeGetLogin(issue.GetUser()), Labels: append([]string{}, extractLabels(issue.Labels)...),
				State: issue.GetState(), UpdatedAt: issue.GetUpdatedAt().Time, URL: issue.GetHTMLURL(), FetchedAt: now}
			if snap.State == "" {
				snap.State = "closed"
			}
			if snap.IsPR {
				// The issues feed carries the merge instant, not the merger.
				snap.MergedAt = issue.GetPullRequestLinks().GetMergedAt().Time
				snap.Merged = !snap.MergedAt.IsZero()
			}
			if snap.Open() {
				// state=closed should never list an open item; never evict on it.
				continue
			}
			if !snap.IsPR {
				c.forgeObserve(repo, forgeIssueItem(issue, 0, nil), forgestate.SourceReadThrough, "")
			}
			out = append(out, snap)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return out, nil
}
