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

// CloseOnMergeAuditOptions bounds one report.
type CloseOnMergeAuditOptions struct {
	// Since is the start of the window. GitHub's issue-list `since` filters on
	// updated_at, so the listing also returns issues closed earlier and
	// updated since; the close time is checked locally against Since.
	Since time.Time
	// MaxIssues caps how many closed issues are inspected (each costs a
	// comments read, and a PR read when the hive's close note names one).
	// 0 means no cap.
	MaxIssues int
}

// CloseOnMergeAudit is one report: the wrong closes found and how much of
// the window was inspected.
type CloseOnMergeAudit struct {
	Items     []CloseOnMergeWrongClose `json:"items"`
	Inspected int                      `json:"inspected"`
	// Truncated is true when MaxIssues stopped the scan before the window
	// was covered; Items then lists only what was inspected.
	Truncated bool `json:"truncated"`
}

// CloseOnMergeWrongCloses lists the issues the hive closed since opts.Since
// with its close-on-merge comment ("Fixed by #N (merged …)") where PR #N did
// not close them (projectbluefin/utah-packages#490: `Refs #490` on #860). It
// only reads: closed issues per watched repo, the close comment, the PR.
func (c *Client) CloseOnMergeWrongCloses(ctx context.Context, opts CloseOnMergeAuditOptions) (CloseOnMergeAudit, error) {
	var out CloseOnMergeAudit
	if c == nil || c.client == nil {
		return out, nil
	}
	ctx = WithRESTCaller(ctx, "hive:close_on_merge_audit")
	var firstErr error
repos:
	for _, repo := range c.activeRepos() {
		owner, name := c.splitRepo(repo)
		full := owner + "/" + name
		list := &gh.IssueListByRepoOptions{State: "closed", Since: opts.Since, ListOptions: gh.ListOptions{PerPage: 100}}
		for page := 0; page < claimSearchMaxPages; page++ {
			issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, list)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			for _, issue := range issues {
				if !closeOnMergeAuditCandidate(issue, opts.Since) {
					continue
				}
				if opts.MaxIssues > 0 && out.Inspected >= opts.MaxIssues {
					out.Truncated = true
					break repos
				}
				out.Inspected++
				if row, ok := c.closeOnMergeWrongClose(ctx, owner, name, full, issue); ok {
					out.Items = append(out.Items, row)
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			list.ListOptions.Page = resp.NextPage
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Repo != out.Items[j].Repo {
			return out.Items[i].Repo < out.Items[j].Repo
		}
		return out.Items[i].Issue < out.Items[j].Issue
	})
	return out, firstErr
}

// closeOnMergeAuditCandidate keeps issues (not PRs) closed at or after since.
func closeOnMergeAuditCandidate(issue *gh.Issue, since time.Time) bool {
	return issue != nil && !issue.IsPullRequest() && strings.EqualFold(issue.GetState(), "closed") && !issue.GetClosedAt().Time.Before(since)
}

func (c *Client) closeOnMergeWrongClose(ctx context.Context, owner, name, full string, issue *gh.Issue) (CloseOnMergeWrongClose, bool) {
	closedAt := issue.GetClosedAt().Time
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
