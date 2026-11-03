package github

// Owner decision (hivecommons/hive, 2026-10-17): issues no longer park at
// stage/085-needs-decision. The issue-side parking stage is
// stage/045-needs-decision; 085 is PR-only. The issue stage reconciler
// re-stages any issue it enumerates (Plan swaps the stale 085 for 045 in
// one write), but issues outside the cycle — a repo the enumeration skips,
// an issue hidden behind an exempt label — would keep wearing a PR stage.
// This one-shot sweep lists every open ISSUE carrying stage/085 across the
// hive's repos and moves it to 045 through the one stage enforcer, exactly
// once per data dir (versioned marker, like the origin-provenance and hold
// migrations). PRs are never touched and the 085 definition is never
// deleted: PRs wear it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	// IssueNeedsDecisionMigrationID names the current migration; the marker
	// and report file names carry the version so a later re-cut runs once
	// more on hives that completed this one.
	IssueNeedsDecisionMigrationID       = "issue-needs-decision-stage-v1"
	issueNeedsDecisionMigrationFileStem = "issue-needs-decision-stage-v1"
)

// IssueNeedsDecisionMigrationOptions configures the one-shot 085 → 045
// re-stage of parked issues.
type IssueNeedsDecisionMigrationOptions struct {
	DataDir    string
	MarkerPath string
	ReportPath string
	Logger     *slog.Logger
}

// IssueNeedsDecisionMigrationReport is written next to the marker.
type IssueNeedsDecisionMigrationReport struct {
	MigrationID string                            `json:"migration_id"`
	GeneratedAt string                            `json:"generated_at"`
	ReportPath  string                            `json:"report_path"`
	From        string                            `json:"from"`
	To          string                            `json:"to"`
	Restaged    []IssueNeedsDecisionMigrationItem `json:"restaged"`
	// SkippedPRs counts open PRs carrying stage/085 the list returned; they
	// are the stage's rightful wearers and are left alone.
	SkippedPRs int      `json:"skipped_prs,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// IssueNeedsDecisionMigrationItem is one open issue moved to 045.
type IssueNeedsDecisionMigrationItem struct {
	Repo    string   `json:"repo"`
	Number  int      `json:"number"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// MigrateIssueNeedsDecisionStage moves every open issue carrying
// stage/085-needs-decision to stage/045-needs-decision, once per data dir.
// A nil report with nil error means the migration already ran. The marker
// is written only when every item migrated, so a failed item retries at the
// next startup.
func (c *Client) MigrateIssueNeedsDecisionStage(ctx context.Context, opts IssueNeedsDecisionMigrationOptions) (*IssueNeedsDecisionMigrationReport, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = defaultHiveHoldMigrationDataDir
	}
	markerPath := opts.MarkerPath
	if markerPath == "" {
		markerPath = filepath.Join(dataDir, issueNeedsDecisionMigrationFileStem+".done")
	}
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(dataDir, issueNeedsDecisionMigrationFileStem+".json")
	}
	if _, err := os.Stat(markerPath); err == nil {
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = c.logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	from, to := stagelabels.NeedsDecision.Label(), stagelabels.IssueNeedsDecision.Label()
	report := &IssueNeedsDecisionMigrationReport{
		MigrationID: IssueNeedsDecisionMigrationID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ReportPath:  reportPath,
		From:        from,
		To:          to,
	}
	ctx = WithRESTCaller(ctx, issueStageRESTCaller)
	items, err := c.openItemsWithLabel(ctx, from)
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	for _, item := range items {
		if item.Type != "issue" {
			report.SkippedPRs++
			continue
		}
		owner, name := c.splitRepo(item.Repo)
		if err := c.ensureStageLabelFamily(ctx, item.Repo); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s#%d: provisioning stage labels: %v", item.Repo, item.Number, err))
			continue
		}
		// The one StageWriter re-reads the issue and plans against that
		// read: the stale 085 (any stage/* not wanted) comes off and 045 goes
		// on; the role and `stalled` the issue wears are kept.
		want := stagelabels.Desired{
			Stage:   stagelabels.IssueNeedsDecision,
			Role:    stagelabels.RoleOf(item.Labels),
			Stalled: issueHasLabel(item.Labels, stagelabels.StalledLabel),
		}
		ref := stageengine.Ref{Owner: owner, Repo: name, Number: item.Number, Kind: stagelabels.IssueItem}
		out, err := c.StageWriter().Apply(ctx, ref, item.Labels, want)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s#%d: %v", item.Repo, item.Number, err))
			continue
		}
		report.Restaged = append(report.Restaged, IssueNeedsDecisionMigrationItem{Repo: item.Repo, Number: item.Number, Added: out.Added, Removed: out.Removed})
	}
	if err := writeIssueNeedsDecisionMigrationReport(reportPath, report); err != nil {
		return report, err
	}
	if len(report.Errors) > 0 {
		logger.Warn("issue needs-decision stage migration finished with errors; it will retry at next startup",
			"restaged", len(report.Restaged), "errors", len(report.Errors), "report", reportPath)
		return report, fmt.Errorf("issue needs-decision stage migration: %d error(s); see %s", len(report.Errors), reportPath)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return report, err
	}
	if err := os.WriteFile(markerPath, []byte(report.GeneratedAt+"\n"), 0o600); err != nil {
		return report, err
	}
	logger.Info("issue needs-decision stage migration complete", "from", from, "to", to, "restaged", len(report.Restaged), "skipped_prs", report.SkippedPRs, "report", reportPath)
	return report, nil
}

func writeIssueNeedsDecisionMigrationReport(path string, report *IssueNeedsDecisionMigrationReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
