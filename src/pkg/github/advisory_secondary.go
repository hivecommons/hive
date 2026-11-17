package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

// Per-repo advisory reports (#18448, opt-in via governor.advisory.per_repo).
//
// A secondary repo's report differs from the primary's in one rule: a report
// a human closed is RESPECTED. The primary is reopened when closed (#4167)
// because it is the hive's one place to look; a secondary report is an extra
// the repo's maintainers may not want, so closing it is the way to say so.
// The lookup therefore considers closed reports too, so a closed one is
// found and reported rather than replaced by a fresh one, on any restart.

// AdvisoryReportMarker is stamped into the body of every per-repo advisory
// report the hive files, so the report is recognisable as the hive's even if
// its hive/advisory label is removed.
const AdvisoryReportMarker = "<!-- hive:advisory-report -->"

// secondaryAdvisoryLookupPageSize bounds the one labelled list (all states)
// that resolves a secondary repo's report.
const secondaryAdvisoryLookupPageSize = 30

// secondaryAdvisoryIssueBody is the body of a per-repo advisory report.
const secondaryAdvisoryIssueBody = "This issue collects advisory findings from Hive agents for this repository.\n\n" +
	"The hive that manages this repository keeps its main advisory report in its primary repo; " +
	"its owner turned on a report in every repo, so the same digest is posted here too.\n\n" +
	"The governor posts periodic digest comments summarizing what advisory agents found.\n\n" +
	"Closing this issue stops the hive posting here. It will not be reopened or replaced.\n\n" +
	AdvisoryReportMarker

// AdvisoryReportClosedError reports that the repo's advisory report exists but
// is closed. For a secondary repo that is final: the hive neither reopens it
// nor files a replacement.
type AdvisoryReportClosedError struct {
	Repo   string
	Number int
}

func (e *AdvisoryReportClosedError) Error() string {
	return fmt.Sprintf("advisory report #%d in %s is closed; it is left closed", e.Number, e.Repo)
}

// isSecondaryAdvisoryReport reports whether issue is a hive advisory report:
// the advisory title together with the hive/advisory label or the hive marker.
func isSecondaryAdvisoryReport(issue *gh.Issue) bool {
	if issue == nil || issue.IsPullRequest() || !isAdvisoryTitle(issue.GetTitle()) {
		return false
	}
	if strings.Contains(issue.GetBody(), AdvisoryReportMarker) {
		return true
	}
	for _, l := range issue.Labels {
		if l.GetName() == advisoryLabelName {
			return true
		}
	}
	return false
}

// pickSecondaryAdvisoryReport chooses among matching reports: the oldest
// OPEN one wins (stable across restarts); with none open, the newest closed
// one is returned with closed=true.
func pickSecondaryAdvisoryReport(issues []*gh.Issue) (num int, closed bool, ok bool) {
	var open, shut []int
	for _, issue := range issues {
		if !isSecondaryAdvisoryReport(issue) {
			continue
		}
		if issue.GetState() == "closed" {
			shut = append(shut, issue.GetNumber())
		} else {
			open = append(open, issue.GetNumber())
		}
	}
	if len(open) > 0 {
		sort.Ints(open)
		return open[0], false, true
	}
	if len(shut) > 0 {
		sort.Ints(shut)
		return shut[len(shut)-1], true, true
	}
	return 0, false, false
}

// EnsureSecondaryAdvisoryIssue finds or creates the advisory report of a
// non-primary repo. An open report is reused; a closed one returns
// *AdvisoryReportClosedError and is never reopened; only when no report exists
// in any state is one created. A lookup failure is returned as an error
// rather than treated as "none", so a search blip never files a duplicate.
func (c *Client) EnsureSecondaryAdvisoryIssue(ctx context.Context, repo string) (int, error) {
	if c == nil {
		return 0, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner = parts[0]
		repo = parts[1]
	}
	full := owner + "/" + repo

	// Labelled reports, open and closed, in one list call.
	labelled, _, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{
		State:       "all",
		Labels:      []string{advisoryLabelName},
		ListOptions: gh.ListOptions{PerPage: secondaryAdvisoryLookupPageSize},
	})
	if err != nil {
		return 0, fmt.Errorf("listing advisory reports in %s: %w", full, err)
	}
	num, closed, ok := pickSecondaryAdvisoryReport(labelled)
	if !ok {
		// A report whose label was removed still carries the title and the
		// marker; the search finds it in any state.
		query := fmt.Sprintf(`repo:%s is:issue in:title "%s"`, full, advisoryTitle)
		result, _, searchErr := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
			ListOptions: gh.ListOptions{PerPage: secondaryAdvisoryLookupPageSize},
		})
		if searchErr != nil {
			return 0, fmt.Errorf("searching advisory reports in %s: %w", full, searchErr)
		}
		num, closed, ok = pickSecondaryAdvisoryReport(result.Issues)
	}
	if ok && closed {
		return 0, &AdvisoryReportClosedError{Repo: full, Number: num}
	}
	if ok {
		c.rememberAdvisoryIssue(full, num)
		c.logger.Info("found existing per-repo advisory report", slog.String("repo", full), slog.Int("number", num))
		return num, nil
	}
	return c.createAdvisoryIssue(ctx, owner, repo, secondaryAdvisoryIssueBody)
}

// AdvisoryIssueOpen reports whether advisory issue n in repo is still open, so
// the per-repo post path can stop writing to a report a human closed. The
// ForgeState cache answers when it can; otherwise one targeted GET.
func (c *Client) AdvisoryIssueOpen(ctx context.Context, repo string, n int) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner = parts[0]
		repo = parts[1]
	}
	full := owner + "/" + repo
	if store := c.advisoryForgeState(); store != nil {
		if it, ok, err := store.ReadThrough(ctx, full, forgestate.KindIssue, n, 0); err == nil && ok && it.Open() {
			return true, nil
		}
	}
	issue, resp, err := c.client.Issues.Get(ctx, owner, repo, n)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("checking advisory report #%d in %s: %w", n, full, err)
	}
	return issue.GetState() != "closed", nil
}
