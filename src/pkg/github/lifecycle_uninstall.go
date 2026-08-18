package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// CloseLifecycleIssueExact closes only the recorded, non-PR issue authored by
// the authenticated Hive writer and carrying exactly one expected lifecycle
// marker. It changes state only; title, body, labels, and unrelated metadata
// are preserved.
func (c *Client) CloseLifecycleIssueExact(ctx context.Context, repository string, number int, marker string) error {
	writer, writerID, err := c.authenticatedLifecycleWriter(ctx)
	if err != nil {
		return err
	}
	return c.closeLifecycleIssueExactOwned(ctx, repository, number, marker, writer, writerID)
}

func (c *Client) CloseLifecycleIssueExactOwned(ctx context.Context, repository string, number int, marker, writerLogin string, writerID int64) error {
	if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
		return fmt.Errorf("immutable lifecycle issue writer ID and login are required")
	}
	return c.closeLifecycleIssueExactOwned(ctx, repository, number, marker, writerLogin, writerID)
}

func (c *Client) closeLifecycleIssueExactOwned(ctx context.Context, repository string, number int, marker, writer string, writerID int64) error {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return err
	}
	if number <= 0 || !isExactHiveLifecycleMarker(marker) {
		return fmt.Errorf("exact lifecycle issue number and marker are required")
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("read lifecycle issue #%d before uninstall close: %w", number, err)
	}
	if issue.GetNumber() != number || !isOwnedLifecycleIssue(issue, marker, writer, writerID) {
		return fmt.Errorf("lifecycle issue #%d is not the exact issue authored by immutable Hive writer %s/%d with marker %q", number, writer, writerID, marker)
	}
	if strings.EqualFold(issue.GetState(), "closed") {
		return nil
	}
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{State: gh.Ptr("closed")}); err != nil {
		return fmt.Errorf("close lifecycle issue #%d for uninstall: %w", number, err)
	}
	confirmed, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("verify lifecycle issue #%d uninstall close: %w", number, err)
	}
	if confirmed.GetNumber() != number || !isOwnedLifecycleIssue(confirmed, marker, writer, writerID) || !strings.EqualFold(confirmed.GetState(), "closed") {
		return fmt.Errorf("lifecycle issue #%d did not close with its exact Hive ownership intact", number)
	}
	return nil
}

// CloseLifecycleIssueByMarkerExact recovers the ambiguous create-response
// window where GitHub may contain the exact Hive-authored issue but the local
// lifecycle has not recorded its number yet. Zero exact owners is a safe no-op;
// multiple owners are an ambiguity and never trigger a write.
func (c *Client) CloseLifecycleIssueByMarkerExact(ctx context.Context, repository, marker string) (int, string, bool, error) {
	writer, writerID, err := c.authenticatedLifecycleWriter(ctx)
	if err != nil {
		return 0, "", false, err
	}
	return c.closeLifecycleIssueByMarkerExactOwned(ctx, repository, marker, writer, writerID)
}

func (c *Client) CloseLifecycleIssueByMarkerExactOwned(ctx context.Context, repository, marker, writerLogin string, writerID int64) (int, string, bool, error) {
	if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
		return 0, "", false, fmt.Errorf("immutable lifecycle issue writer ID and login are required")
	}
	return c.closeLifecycleIssueByMarkerExactOwned(ctx, repository, marker, writerLogin, writerID)
}

func (c *Client) closeLifecycleIssueByMarkerExactOwned(ctx context.Context, repository, marker, writer string, writerID int64) (int, string, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", false, err
	}
	if !isExactHiveLifecycleMarker(marker) {
		return 0, "", false, fmt.Errorf("exact lifecycle marker is required")
	}
	issues, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer, writerID)
	if err != nil {
		return 0, "", false, err
	}
	if len(issues) == 0 {
		return 0, "", false, nil
	}
	if len(issues) != 1 {
		return 0, "", false, fmt.Errorf("multiple exact Hive lifecycle issues claim marker %q", marker)
	}
	issue := issues[0]
	if err := c.closeLifecycleIssueExactOwned(ctx, repository, issue.GetNumber(), marker, writer, writerID); err != nil {
		return 0, "", false, err
	}
	return issue.GetNumber(), issue.GetHTMLURL(), true, nil
}
