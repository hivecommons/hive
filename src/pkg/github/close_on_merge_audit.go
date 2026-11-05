package github

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// CloseOnMergeWrongClose is one issue close-on-merge closed on a PR that, by
// the closing-keyword decision the issue's coverage comment states
// (prLeavesIssueOpen), left it open, or that the issue's labels said still
// had work left. It is a report for maintainers to reopen by hand; the hive
// never reopens these itself.
type CloseOnMergeWrongClose struct {
	Repo     string    `json:"repo"`
	Issue    int       `json:"issue"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	PR       int       `json:"pr"`
	ClosedAt time.Time `json:"closed_at"`
	// Reason is "pr_leaves_issue_open" (the PR only references the issue or
	// says it stays open) or "issue_marked_open" (no closing keyword and the
	// issue carries hive/verified-open or hive/partially-addressed today).
	Reason string `json:"reason"`
}

var closeOnMergeFixedByRE = regexp.MustCompile(`Fixed by #(\d+) \(merged`)

// CloseOnMergeWrongCloses lists the issues the hive closed since `since` with
// its close-on-merge comment ("Fixed by #N (merged …)") where PR #N did not
// close them (projectbluefin/utah-packages#490: `Refs #490` on #860). It only
// reads: closed issues per watched repo, the close comment, the PR.
func (c *Client) CloseOnMergeWrongCloses(ctx context.Context, since time.Time) ([]CloseOnMergeWrongClose, error) {
	if c == nil || c.client == nil {
		return nil, nil
	}
	ctx = WithRESTCaller(ctx, "hive:close_on_merge_audit")
	var out []CloseOnMergeWrongClose
	var firstErr error
	for _, repo := range c.activeRepos() {
		owner, name := c.splitRepo(repo)
		full := owner + "/" + name
		opts := &gh.IssueListByRepoOptions{State: "closed", Since: since, ListOptions: gh.ListOptions{PerPage: 100}}
		for page := 0; page < claimSearchMaxPages; page++ {
			issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, opts)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			for _, issue := range issues {
				if row, ok := c.closeOnMergeWrongClose(ctx, owner, name, full, since, issue); ok {
					out = append(out, row)
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.ListOptions.Page = resp.NextPage
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Issue < out[j].Issue
	})
	return out, firstErr
}

func (c *Client) closeOnMergeWrongClose(ctx context.Context, owner, name, full string, since time.Time, issue *gh.Issue) (CloseOnMergeWrongClose, bool) {
	if issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "closed") {
		return CloseOnMergeWrongClose{}, false
	}
	closedAt := issue.GetClosedAt().Time
	if closedAt.Before(since) {
		return CloseOnMergeWrongClose{}, false
	}
	pr := c.closeOnMergeCommentPR(ctx, owner, name, issue.GetNumber(), closedAt)
	if pr <= 0 {
		return CloseOnMergeWrongClose{}, false
	}
	merged, err := c.getPullRequestForCloseOnMerge(ctx, full, pr)
	if err != nil || merged == nil {
		return CloseOnMergeWrongClose{}, false
	}
	prRepo := full
	if base := merged.GetBase().GetRepo().GetFullName(); base != "" {
		prRepo = base
	}
	text := merged.GetTitle() + "\n" + merged.GetBody()
	row := CloseOnMergeWrongClose{Repo: full, Issue: issue.GetNumber(), Title: issue.GetTitle(), URL: issue.GetHTMLURL(), PR: pr, ClosedAt: closedAt}
	switch {
	case prLeavesIssueOpen(text, prRepo, full, issue.GetNumber()):
		row.Reason = "pr_leaves_issue_open"
	case closeOnMergeIssueMarkedOpen(issue):
		if _, closing := FindClosingKeywordReference(text, prRepo, full, issue.GetNumber()); closing {
			return CloseOnMergeWrongClose{}, false
		}
		row.Reason = "issue_marked_open"
	default:
		return CloseOnMergeWrongClose{}, false
	}
	return row, true
}

// closeOnMergeCommentPR finds the hive's close-on-merge comment posted around
// the close and returns the PR it names, or 0.
func (c *Client) closeOnMergeCommentPR(ctx context.Context, owner, name string, number int, closedAt time.Time) int {
	opts := &gh.IssueListCommentsOptions{Since: gh.Ptr(closedAt.Add(-10 * time.Minute)), ListOptions: gh.ListOptions{PerPage: closeOnMergeCommentPageSize}}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, name, number, opts)
	if err != nil {
		return 0
	}
	for _, cm := range comments {
		body := cm.GetBody()
		if !c.isHiveAccount(cm.GetUser()) || !strings.Contains(body, CloseOnMergeMarker) {
			continue
		}
		if m := closeOnMergeFixedByRE.FindStringSubmatch(body); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}
