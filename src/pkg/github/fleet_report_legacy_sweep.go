package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// Standalone "Fleet report: …" issues were retired by hivecommons/hive#13470:
// every hive-filed report is a `[selfreport]` tracker now. A spoke still on a
// pre-#13470 build may write to the report repo directly (its own credential,
// no hub relay in between), so the hub-side refusal alone cannot stop one
// from landing (hivecommons/hive#13567). This sweep is the last line: the
// hive that watches the report repo closes any open bot-authored issue in
// the retired title format with a pointer at #13470.

const (
	// LegacyFleetReportSweepInterval is how often the governor runs the sweep.
	LegacyFleetReportSweepInterval = time.Hour
	// legacyFleetReportSweepMaxCloses bounds one pass; the next pass picks
	// up the rest.
	legacyFleetReportSweepMaxCloses = 5
	// legacyFleetReportSweepPageSize bounds the one Search call a pass costs.
	legacyFleetReportSweepPageSize = 20
	// legacyFleetReportCloseMarker tags the hive's closing comment.
	legacyFleetReportCloseMarker = "<!-- hive-fleet-legacy-standalone-close -->"
	// legacyFleetReportFingerprintMarker is the body marker every hive-filed
	// report carries; a human never writes it, so it is the authorship proof
	// alongside the Bot account type.
	legacyFleetReportFingerprintMarker = "<!-- hive-fleet-fingerprint:"
	// legacyFleetReportStateReasonReopened is GitHub's state_reason for an
	// open issue a person reopened; such an issue is left alone.
	legacyFleetReportStateReasonReopened = "reopened"
	// legacyFleetReportCloseReason is the audit reason on the close.
	legacyFleetReportCloseReason = "legacy_standalone"
	// legacyFleetReportStateReasonNotPlanned is the state_reason the close
	// carries: the report was retired, not resolved.
	legacyFleetReportStateReasonNotPlanned = "not_planned"
)

// LegacyFleetReportSweepOptions configures SweepLegacyFleetReportIssues.
type LegacyFleetReportSweepOptions struct {
	// Repo is the report repo; empty means hivecommons/hive.
	Repo string
	// MaxCloses caps closes per pass; <=0 means legacyFleetReportSweepMaxCloses.
	MaxCloses int
	// Audit, when set, receives one event per closed issue.
	Audit func(event LegacyFleetReportSweepEvent)
}

// LegacyFleetReportSweepEvent is one issue the sweep closed.
type LegacyFleetReportSweepEvent struct {
	Repo   string
	Number int
	Title  string
	Author string
}

// LegacyFleetReportSweepResult summarises one pass.
type LegacyFleetReportSweepResult struct {
	Scanned int
	Skipped int
	Closed  []LegacyFleetReportSweepEvent
	Errors  []string
}

func (o LegacyFleetReportSweepOptions) repo() string {
	if r := strings.TrimSpace(o.Repo); r != "" {
		return r
	}
	return fleetReportRepo
}

func (o LegacyFleetReportSweepOptions) maxCloses() int {
	if o.MaxCloses > 0 {
		return o.MaxCloses
	}
	return legacyFleetReportSweepMaxCloses
}

// legacyFleetReportCloseComment is the closing comment, pointing at the
// retirement issue and the tracker format that replaced the standalone one.
func legacyFleetReportCloseComment() string {
	return legacyFleetReportCloseMarker + "\nClosing: standalone `" + fleetreport.LegacyStandaloneTitlePrefix + "` issues were retired by hivecommons/hive#13470. " +
		"Runtime errors and ACMM shortfalls now land in the per-hive `" + strings.TrimSpace(fleetreport.SelfReportTitlePrefix) + "` tracker issue with hive identity, owners, spoke link and diagnostics attached. " +
		"This one came from a spoke still running a pre-#13470 build; upgrading it stops the reports."
}

// legacyFleetReportIssueEligible reports whether issue is a hive-filed
// standalone report the sweep may close: the retired title format, the
// fingerprint marker in the body, a Bot author, and not reopened by a person.
func legacyFleetReportIssueEligible(issue *gh.Issue) (string, bool) {
	if issue == nil || issue.GetPullRequestLinks() != nil {
		return "not an issue", false
	}
	if !fleetreport.IsLegacyStandaloneTitle(issue.GetTitle()) {
		return "title is not the retired format", false
	}
	if !strings.Contains(issue.GetBody(), legacyFleetReportFingerprintMarker) {
		return "no fleet fingerprint marker", false
	}
	user := issue.GetUser()
	if !strings.EqualFold(user.GetType(), "Bot") && !strings.HasSuffix(strings.ToLower(user.GetLogin()), "[bot]") {
		return "author is not a bot", false
	}
	if strings.EqualFold(issue.GetStateReason(), legacyFleetReportStateReasonReopened) {
		return "reopened by a person", false
	}
	if strings.Contains(issue.GetBody(), legacyFleetReportCloseMarker) {
		return "already closed by the sweep", false
	}
	return "", true
}

// SweepLegacyFleetReportIssues closes open standalone "Fleet report:" issues
// in the report repo (one Search call, at most MaxCloses closes). It only
// touches issues a bot filed that carry the fleet fingerprint marker; one a
// person reopened is left alone.
func (c *Client) SweepLegacyFleetReportIssues(ctx context.Context, opts LegacyFleetReportSweepOptions) (*LegacyFleetReportSweepResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	repo := opts.repo()
	result := &LegacyFleetReportSweepResult{}
	q := fmt.Sprintf("repo:%s is:issue is:open in:title %q", repo, fleetreport.LegacyStandaloneTitlePrefix)
	found, _, err := c.client.Search.Issues(ctx, q, &gh.SearchOptions{Sort: "created", Order: "desc", ListOptions: gh.ListOptions{PerPage: legacyFleetReportSweepPageSize}})
	if err != nil {
		return result, fmt.Errorf("searching legacy fleet report issues: %w", err)
	}
	for _, issue := range found.Issues {
		result.Scanned++
		if len(result.Closed) >= opts.maxCloses() {
			result.Skipped++
			continue
		}
		if reason, ok := legacyFleetReportIssueEligible(issue); !ok {
			result.Skipped++
			c.logger.Debug("legacy fleet report sweep: skipping issue", "repo", repo, "issue", issue.GetNumber(), "reason", reason)
			continue
		}
		num := issue.GetNumber()
		if err := c.CreateIssueComment(ctx, repo, num, legacyFleetReportCloseComment()); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("#%d comment: %v", num, err))
			continue
		}
		if err := c.CloseIssue(ctx, repo, num, IssueCloseOptions{StateReason: legacyFleetReportStateReasonNotPlanned}); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("#%d close: %v", num, err))
			continue
		}
		event := LegacyFleetReportSweepEvent{Repo: repo, Number: num, Title: issue.GetTitle(), Author: issue.GetUser().GetLogin()}
		result.Closed = append(result.Closed, event)
		c.recordWriteAudit(AuditActionFleetReportRecovered, hiveWriteMeta(),
			WriteTarget{Repo: repo, Number: num},
			"closed", "true", "reason", legacyFleetReportCloseReason)
		c.logger.Info("legacy fleet report sweep: closed standalone issue", "repo", repo, "issue", num, "title", issue.GetTitle(), "author", event.Author)
		if opts.Audit != nil {
			opts.Audit(event)
		}
	}
	return result, nil
}
