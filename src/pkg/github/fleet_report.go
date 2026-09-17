package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const fleetReportRepo = "hivecommons/hive"

type FleetReportResult struct {
	Number       int
	URL          string
	Created      bool
	Commented    bool
	ReactionSent bool
}

// EnsureFleetReport writes or updates a fleet report in hivecommons/hive. It
// reuses the existing CreateIssue/CreateIssueComment paths for body scrubbing
// and ioscan canary checks; only the deterministic fingerprint search and +1
// reaction are implemented here.
func (c *Client) EnsureFleetReport(ctx context.Context, report fleetreport.Report) (FleetReportResult, error) {
	if c == nil || c.client == nil {
		return FleetReportResult{}, ErrNoGitHubClient
	}
	if strings.TrimSpace(report.Fingerprint) == "" {
		return FleetReportResult{}, fmt.Errorf("fleet report fingerprint is required")
	}
	if existing, err := c.findFleetReportIssue(ctx, report.Fingerprint); err != nil {
		return FleetReportResult{}, fmt.Errorf("searching fleet report issue: %w", err)
	} else if existing != nil {
		if err := c.CreateIssueComment(ctx, fleetReportRepo, existing.GetNumber(), report.Body); err != nil {
			return FleetReportResult{}, fmt.Errorf("commenting on fleet report: %w", err)
		}
		reacted := false
		if _, _, err := c.client.Reactions.CreateIssueReaction(ctx, "hivecommons", "hive", existing.GetNumber(), "+1"); err == nil || isAlreadyExists(err) {
			reacted = true
		} else {
			c.logger.Warn("fleet report: could not add +1 reaction", "issue", existing.GetNumber(), "error", err)
		}
		return FleetReportResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Commented: true, ReactionSent: reacted}, nil
	}

	res, err := c.CreateIssue(ctx, fleetReportRepo, report.Title, report.Body, report.Labels)
	if err != nil {
		return FleetReportResult{}, err
	}
	return FleetReportResult{Number: res.Number, URL: res.URL, Created: !res.AlreadyExisted, Commented: res.AlreadyExisted}, nil
}

func (c *Client) PostFleetReportRecovery(ctx context.Context, issueNumber int, report fleetreport.Report, closeIfOwner bool) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if issueNumber <= 0 {
		return fmt.Errorf("fleet report issue number is required")
	}
	if err := c.CreateIssueComment(ctx, fleetReportRepo, issueNumber, report.Body); err != nil {
		return err
	}
	if closeIfOwner {
		_, _, err := c.client.Issues.Edit(ctx, "hivecommons", "hive", issueNumber, &gh.IssueRequest{State: gh.Ptr("closed")})
		return err
	}
	return nil
}

func (c *Client) findFleetReportIssue(ctx context.Context, fingerprint string) (*gh.Issue, error) {
	query := fmt.Sprintf("repo:%s is:issue is:open %q", fleetReportRepo, "hive-fleet-fingerprint:"+logscrub.ScrubString(fingerprint))
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 10}})
	if err != nil {
		return nil, err
	}
	for _, issue := range result.Issues {
		if issue.GetPullRequestLinks() != nil {
			continue
		}
		return issue, nil
	}
	return nil, nil
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if ghErr, ok := err.(*gh.ErrorResponse); ok && ghErr.Response != nil {
		return ghErr.Response.StatusCode == http.StatusUnprocessableEntity
	}
	return strings.Contains(strings.ToLower(err.Error()), "already")
}

func (c *Client) FleetReportIssue(ctx context.Context, fingerprint string) (FleetReportResult, bool, error) {
	if c == nil || c.client == nil {
		return FleetReportResult{}, false, ErrNoGitHubClient
	}
	issue, err := c.findFleetReportIssue(ctx, fingerprint)
	if err != nil {
		return FleetReportResult{}, false, err
	}
	if issue == nil {
		return FleetReportResult{}, false, nil
	}
	return FleetReportResult{Number: issue.GetNumber(), URL: issue.GetHTMLURL()}, true, nil
}
