package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// clarityGateResumeSweepInterval spaces the clarity gate's resume passes. An
// author who answered should not wait long, but each pass reads comments on
// every gate-parked issue, so it runs at the un-park sweep's cadence.
const clarityGateResumeSweepInterval = issueUnparkSweepInterval

// runClarityGateResumeSweepIfDue lifts the clarity gate's `needs-spec` once
// the issue author (or a maintainer) answers its questions
// (hivecommons/hive#13588). It runs only while
// runs.triage.clarity_gate.enabled; every eligibility rule lives in
// SweepClarityGateResume, and this function is only the scheduler and the
// dashboard audit sink.
func runClarityGateResumeSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Runs.Triage.ClarityGate.Enabled {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < clarityGateResumeSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepClarityGateResume(ctx, github.ClarityGateResumeOptions{
		Label:      cfg.Project.IssueFilter.HardSuppressLabels.EffectiveNeedsSpec()[0],
		MaxActions: github.DefaultClarityGateResumeMaxActions,
		Audit: func(event github.ClarityGateResumeEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s, label=%s", event.Repo, event.Number, event.Actor, event.Label)
			dashSrv.AuditLogRecord("system", "clarity-gate-resumed", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("clarity gate resume sweep failed", "error", err)
		return
	}
	if result != nil && (len(result.Resumed) > 0 || result.Skipped > 0) {
		logger.Info("clarity gate resume sweep complete", "seen", result.Seen, "resumed", len(result.Resumed), "skipped", result.Skipped)
	}
}
