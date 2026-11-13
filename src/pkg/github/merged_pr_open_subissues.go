package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A merged PR does not mark done or close an issue that has an open
// sub-issue.
//
// Two paths write "done" on an issue because a PR that names it with a
// closing keyword merged: close-on-merge (close_on_merge.go) and the issue
// stage sweep's merged-PR close (handleMergedPRIssueStage). Both posted a
// "Fixed by #N" comment and closed the issue (close-on-merge, behind a
// confirmation gate, posted the awaiting comment and added hive/likely-done
// instead) whatever the issue's sub-issues said. An issue whose remaining
// work had been moved into three open sub-issues was told it was fixed and
// closed with all three still open. In close-on-merge the comment is posted
// before the close and carries the close-on-merge marker, which later sweeps
// read as "already handled", so refusing only the close would have left a
// false note and nothing to correct it; both paths therefore write nothing.
//
// Neither path makes a request to find out. Close-on-merge reads GitHub's
// own `sub_issues_summary` block off the issue payload it fetches anyway:
// the single-issue GET (getIssueWithBlockedCount) when it reads the issue
// itself, the open-issue listing (closeOnMergeListOpenIssuesPage) when a
// sweep hands it a listed row. The stage sweep reads the enumeration row
// (IssueStageInput.Children, which childrenOf fills from that same summary
// or, when GitHub sent none, from the split ledger and parent markers). So
// nothing here can fail on its own: a read that fails is the issue read or
// the listing, and both keep the handling they already had. An issue whose
// payload or row names no sub-issues is handled exactly as before.
//
// The parent is closed later by the issue stage sweep, once every sub-issue
// is closed (closeDecomposedParent). A close-on-merge claim settles like any
// other skip, so the ledger sweep re-reads it once per recheck window and,
// with the sub-issues all closed by then and the parent still open, comments
// and closes as it always did.

// closeOnMergeListOpenIssuesPage reads one page of a repo's open issues
// (state=open, 100 per page) and keeps each parent's sub-issue summary,
// which go-github's Issues.ListByRepo drops. It is the request
// listOpenIssuesWithBlockedCounts makes for the enumeration, one page at a
// time so the sweeps keep their own page cap. The returned issues are what
// Issues.ListByRepo would have decoded; summaries holds only parents
// (total > 0), keyed by issue number.
func (c *Client) closeOnMergeListOpenIssuesPage(ctx context.Context, owner, repoName string, page int) ([]*gh.Issue, map[int]IssueChildren, *gh.Response, error) {
	if page <= 0 {
		page = 1
	}
	q := url.Values{}
	q.Set("state", "open")
	q.Set("per_page", "100")
	q.Set("page", strconv.Itoa(page))
	req, err := c.client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/issues?%s", owner, repoName, q.Encode()), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	var rows []*issueWithDependencies
	resp, err := c.client.Do(ctx, req, &rows)
	if err != nil {
		return nil, nil, resp, err
	}
	issues := make([]*gh.Issue, 0, len(rows))
	summaries := map[int]IssueChildren{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		issue := row.Issue
		issues = append(issues, &issue)
		if row.SubIssuesSummary != nil && row.SubIssuesSummary.Total > 0 {
			summaries[issue.GetNumber()] = *row.SubIssuesSummary
		}
	}
	return issues, summaries, resp, nil
}

// closeOnMergeListedChildren is the sub-issue summary a listing carried for
// issue number, or nil when it carried none.
func closeOnMergeListedChildren(summaries map[int]IssueChildren, number int) *IssueChildren {
	summary, ok := summaries[number]
	if !ok {
		return nil
	}
	return &summary
}

// closeOnMergeHeldForOpenSubIssues reports whether close-on-merge must write
// nothing on res's issue because it has an open sub-issue, and records that
// verdict on res. It is asked right before the first "done" write of each
// path: the awaiting-confirmation comment (and hive/likely-done), and the
// "Fixed by" comment (and the close).
//
// The verdict is CloseOnMergeSkipped, like tracking_issue: it settles the
// ledger claim, so the sweep stops re-reading the parent on every pass and
// looks again once per recheck window. children is the summary read with
// the issue; nil, or one with nothing open, holds nothing.
func closeOnMergeHeldForOpenSubIssues(res *CloseOnMergeResult, pr int, children *IssueChildren, opts CloseOnMergeOptions) bool {
	if children == nil || children.Open() == 0 {
		return false
	}
	res.Action, res.Reason = CloseOnMergeSkipped, "open_sub_issues"
	if opts.Logger != nil {
		// A verdict, not a failed step, so INFO rather than WARN.
		opts.Logger.Info("close-on-merge: issue has open sub-issues; leaving it open and writing nothing",
			"repo", res.Repo, "issue", res.Issue, "pr", pr, "open", children.Open(), "total", children.Total)
	}
	return true
}

// mergedPRParentHoldLogged remembers, per issue and PR, that the stage
// sweep already said at INFO that it is leaving a parent open. The sweep
// reaches the same parent every pass until its sub-issues close; the first
// pass of a process says so at INFO and later ones at DEBUG.
var mergedPRParentHoldLogged coveredByPRLabelState

// mergedPRIssueStageHeldForOpenChildren reports whether the issue stage
// sweep must leave the issue alone, with no "Fixed by" note and no close, because
// the enumeration row shows an open sub-issue. in.Claim is the merged
// closing claim handleMergedPRIssueStage is about to act on.
func mergedPRIssueStageHeldForOpenChildren(client *Client, in IssueStageInput, logger *slog.Logger) bool {
	if in.Children == nil || in.Children.Open() == 0 {
		return false
	}
	if logger == nil {
		return true
	}
	pr := 0
	if in.Claim != nil {
		pr = in.Claim.PRNumber
	}
	owner, name := client.splitRepo(in.Repo)
	key := fmt.Sprintf("%s/%s#%d pr=%d", owner, name, in.Number, pr)
	const msg = "issue stage labels: merged PR names a parent with open sub-issues; leaving it open and writing nothing"
	if mergedPRParentHoldLogged.has(key) {
		logger.Debug(msg, "repo", in.Repo, "issue", in.Number, "pr", pr, "open", in.Children.Open(), "total", in.Children.Total)
		return true
	}
	mergedPRParentHoldLogged.mark(key)
	logger.Info(msg, "repo", in.Repo, "issue", in.Number, "pr", pr, "open", in.Children.Open(), "total", in.Children.Total)
	return true
}

// mergedPRHeldParentFallIsProgress reports whether the issue stage sweep is
// moving a parent with an open sub-issue to its unclaimed stage because the
// PR that claimed it with a closing keyword merged. Such a parent used to be
// closed at this point; now that it is left open, its fall from the PR's
// stage (030 or 040) to 010 is the same kind of fall the store already
// counts as progress for an issue wearing hive/partially-addressed
// (stageengine's issueProgress): the work landed and nothing was lost. The
// sweep passes this to Store.ObserveProgress, so no regression is booked,
// no regression note is posted, and a regression another writer booked for
// the same fall is cleared. The stages are the three issueProgress accepts.
//
// in.Claim is still the merged claim here: the sweep clears the claim's
// bucket and holder before deriving the stage, not the claim context.
func mergedPRHeldParentFallIsProgress(in IssueStageInput, stage stagelabels.Stage) bool {
	if stage != stagelabels.Actionable && stage != stagelabels.TriageNeeded && stage != stagelabels.OpenForDiscussion {
		return false
	}
	if in.Claim == nil || !in.Claim.MergedPR || !in.Claim.Closing {
		return false
	}
	return in.Children != nil && in.Children.Open() > 0
}
