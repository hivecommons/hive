package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// sharedCIIssuesInterval spaces the shared-CI issue sweep. It reads the
// governor's last snapshot, so it costs GitHub calls only in repositories
// where a check is failing across several PRs.
const sharedCIIssuesInterval = 15 * time.Minute

// runSharedCIIssuesSweep files or updates one `[shared-ci]` tracking issue per
// check failing across a repository (github.SweepSharedCIIssues), from the
// latest actionable snapshot. Gates, dedupe and caps live there; this is the
// config switch and the dashboard audit sink.
func runSharedCIIssuesSweep(ctx context.Context, ghClient *github.Client, cfg *config.Config, actionable *github.ActionableResult, tracker *github.SharedCIIssueTracker, dashSrv *dashboard.Server, logger *slog.Logger) {
	if ghClient == nil || actionable == nil {
		return
	}
	if cfg != nil && !cfg.SharedCIIssues.IsEnabled() {
		return
	}
	minPRs := 0
	if cfg != nil {
		minPRs = cfg.SharedCIIssues.MinPRsOrDefault()
	}
	result, err := ghClient.SweepSharedCIIssues(ctx, actionable, github.SharedCIIssueOptions{
		MinPRs:  minPRs,
		Tracker: tracker,
		Audit: func(event github.SharedCIIssueEvent) {
			if dashSrv == nil {
				return
			}
			prs := make([]string, len(event.PRs))
			for i, n := range event.PRs {
				prs[i] = "#" + strconv.Itoa(n)
			}
			detail := fmt.Sprintf("repo=%s, check=%s, issue=%d, prs=%s, on_main=%t",
				event.Repo, event.Check, event.Issue, strings.Join(prs, ","), event.OnMain)
			dashSrv.AuditLogRecord("system", "shared-ci-issue-"+event.Action, detail, "", event.Repo, event.Issue)
		},
	})
	if err != nil {
		if logger != nil {
			logger.Warn("shared-ci issue sweep failed", "error", err)
		}
		return
	}
	if len(result.Events) > 0 && logger != nil {
		logger.Info("shared-ci issue sweep complete", "clusters", result.Clusters, "written", len(result.Events), "skipped", result.Skipped)
	}
}
