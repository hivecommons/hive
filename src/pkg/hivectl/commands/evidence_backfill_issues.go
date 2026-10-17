package commands

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/spf13/cobra"
)

type backfillIssue struct {
	Number      int        `json:"number"`
	Body        string     `json:"body"`
	CreatedAt   time.Time  `json:"created_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	PullRequest *struct{}  `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func issueWeekKey(t time.Time) string {
	year, week := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// listIssues scans all issues created before until. Older issues can close in
// the requested range, so stopping at since would undercount closes.
func (f *backfillFetcher) listIssues(ctx context.Context, repo string, until time.Time) ([]backfillIssue, error) {
	var out []backfillIssue
	for page := 1; ; page++ {
		q := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"desc"},
			"per_page": {strconv.Itoa(evidenceBackfillPerPage)}, "page": {strconv.Itoa(page)}}
		var batch []backfillIssue
		if err := f.get(ctx, "/repos/"+repo+"/issues", q, &batch); err != nil {
			return nil, err
		}
		for _, issue := range batch {
			if issue.PullRequest == nil && issue.CreatedAt.Before(until) {
				out = append(out, issue)
			}
		}
		if len(batch) < evidenceBackfillPerPage {
			return out, nil
		}
	}
}

func issueWeeks(issues []backfillIssue, since, until time.Time, agents []string) []evidence.IssueWeek {
	weeks := map[string]*evidence.IssueWeek{}
	for day := since.UTC().Truncate(24 * time.Hour); day.Before(until); day = day.AddDate(0, 0, 7) {
		// Advance by seven days only after establishing every touched ISO week.
		for d := day; d.Before(day.AddDate(0, 0, 7)) && d.Before(until); d = d.AddDate(0, 0, 1) {
			key := issueWeekKey(d)
			if weeks[key] == nil {
				weeks[key] = &evidence.IssueWeek{Week: key, Opened: map[string]int{"human": 0, "agent": 0, "bot": 0}, Events: []evidence.IssueEvent{}}
			}
		}
	}
	for _, issue := range issues {
		if !issue.CreatedAt.Before(since) && issue.CreatedAt.Before(until) {
			pr := backfillPR{Body: issue.Body}
			pr.User = issue.User
			author, _ := backfillAuthor(pr, agents)
			week := weeks[issueWeekKey(issue.CreatedAt)]
			week.Opened[author.Kind]++
			week.Events = append(week.Events, evidence.IssueEvent{At: issue.CreatedAt, Action: evidence.IssueActionOpened, Kind: author.Kind})
		}
		if issue.ClosedAt != nil && !issue.ClosedAt.Before(since) && issue.ClosedAt.Before(until) {
			week := weeks[issueWeekKey(*issue.ClosedAt)]
			week.Closed++
			week.Events = append(week.Events, evidence.IssueEvent{At: *issue.ClosedAt, Action: evidence.IssueActionClosed})
		}
	}
	out := make([]evidence.IssueWeek, 0, len(weeks))
	for _, week := range weeks {
		year, number := 0, 0
		_, _ = fmt.Sscanf(week.Week, "%d-W%d", &year, &number)
		start := isoWeekStart(year, number)
		week.CoveredFrom, week.CoveredTo = start, start.AddDate(0, 0, 7)
		if since.After(week.CoveredFrom) {
			week.CoveredFrom = since
		}
		if until.Before(week.CoveredTo) {
			week.CoveredTo = until
		}
		out = append(out, *week)
	}
	sortIssueWeeks(out)
	return out
}

func isoWeekStart(year, week int) time.Time {
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	return jan4.AddDate(0, 0, -int(jan4.Weekday()+6)%7+(week-1)*7)
}

func sortIssueWeeks(weeks []evidence.IssueWeek) {
	sort.Slice(weeks, func(i, j int) bool { return weeks[i].Week < weeks[j].Week })
}

func newEvidenceBackfillIssuesCommand(env *commandEnv) *cobra.Command {
	var repo, sinceRaw, untilRaw, root, apiBase, tokenEnv string
	var agents []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "backfill-issues --repo owner/repo --since DATE",
		Short: "Backfill weekly GitHub issue counts for work balance (record-only)",
		Args:  argsNone(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := evidence.IssueWeekPath("", repo, "2026-W01"); err != nil {
				return &usageError{message: "--repo must be owner/repo"}
			}
			if sinceRaw == "" {
				return &usageError{message: "--since is required"}
			}
			since, err := parseBackfillDate(sinceRaw)
			if err != nil {
				return err
			}
			until := time.Now().UTC()
			if untilRaw != "" {
				until, err = parseBackfillDate(untilRaw)
				if err != nil {
					return err
				}
			}
			if !until.After(since) {
				return &usageError{message: "--until must be after --since"}
			}
			if root == "" {
				root = ghpkg.ReviewEvidenceRoot
			}
			fetcher := newBackfillFetcher(apiBase, backfillToken(tokenEnv))
			issues, err := fetcher.listIssues(cmd.Context(), repo, until)
			if err != nil {
				return fmt.Errorf("listing issues: %w", err)
			}
			weeks := issueWeeks(issues, since, until, agents)
			if !dryRun {
				for _, week := range weeks {
					if err := evidence.WriteIssueWeek(root, repo, week); err != nil {
						return err
					}
				}
			}
			return env.print(cmd, map[string]any{"repo": repo, "issues": len(issues), "weeks": len(weeks), "api_calls": fetcher.calls, "dry_run": dryRun})
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "repository as owner/repo (required)")
	f.StringVar(&sinceRaw, "since", "", "include issue events at or after this date (required)")
	f.StringVar(&untilRaw, "until", "", "exclude issue events at or after this date (default: now)")
	f.StringVar(&root, "root", "", "evidence root (default: /data/evidence)")
	f.BoolVar(&dryRun, "dry-run", false, "fetch and count without writing files")
	f.StringArrayVar(&agents, "agent-login", nil, "login to classify as an agent (repeatable)")
	f.StringVar(&apiBase, "github-api", evidenceBackfillAPIDefault, "GitHub REST API base URL")
	f.StringVar(&tokenEnv, "github-token-env", evidenceBackfillTokenEnv, "environment variable holding the GitHub token")
	return cmd
}
