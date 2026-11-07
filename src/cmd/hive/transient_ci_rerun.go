package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// transientCIRerunMaxPerPass bounds the re-run calls one eval cycle makes.
const transientCIRerunMaxPerPass = 10

// transientCIRerunner re-runs a transient-triaged PR's failed jobs;
// *github.Client implements it.
type transientCIRerunner interface {
	RerunTransientCIFailures(ctx context.Context, pr github.PullRequest) (github.TransientCIRerunOutcome, error)
}

// rerunTransientTriagedPRs is the fix loop's answer to a red hive-authored PR
// whose every failing check run an agent triaged as infrastructure, not this
// change (`hive-ci-transient` markers, github.PullRequest.TransientCITriaged):
// it re-runs the failed jobs once per head through rerun-failed-jobs, and
// never kicks a fixer or pushes a commit for it. On
// projectbluefin/common#1647 the fix loop instead kicked the owning agent six
// times on one head, each kick answered with "infrastructure, not this
// change", escalated the PR to a person, and a lane then pushed an empty
// "retrigger CI" commit.
//
// It runs after recordRedStaleness (which records the triaged head in the
// ledger) and before reapStuckRedPRs, whose kicks deliverReEngagement holds
// while a re-run is owed. audit, when non-nil, receives one line per PR the
// hive re-ran.
func rerunTransientTriagedPRs(
	ctx context.Context,
	cfg *config.Config,
	rerunner transientCIRerunner,
	actionable *github.ActionableResult,
	escalatedPRs map[string]bool,
	audit func(repo string, number int, detail string),
	logger *slog.Logger,
) int {
	if cfg == nil || cfg.Escalation.Disabled || rerunner == nil || actionable == nil {
		return 0
	}
	store := getEscalationStore()
	attempted := 0
	for _, pr := range actionable.PRs.Items {
		if attempted >= transientCIRerunMaxPerPass {
			break
		}
		if !isHiveAgentAuthor(cfg, pr.Author) || !pr.TransientCITriaged || pr.CIChecksRunning || !pr.HasFailingRequiredCheck() {
			continue
		}
		o := escalationObservation(cfg, pr, nil)
		key := escalation.Key(o.Repo, o.Number)
		if escalatedPRs[key] || escalation.HasNeedsHumanLabel(pr.Labels) {
			continue
		}
		if !store.TransientRerunOwed(o.Repo, o.Number, o.HeadSHA) {
			continue
		}
		attempted++
		outcome, err := rerunner.RerunTransientCIFailures(ctx, pr)
		if err != nil {
			if logger != nil {
				logger.Warn("transient CI rerun: could not list failed runs; retrying next pass",
					"repo", o.Repo, "pr", o.Number, "head", o.HeadSHA, "error", err)
			}
			continue
		}
		if !outcome.Settled {
			continue
		}
		store.RecordTransientRerun(o.Repo, o.Number, o.HeadSHA)
		if len(outcome.RunIDs) == 0 {
			if logger != nil {
				logger.Info("transient CI rerun skipped; the fix loop resumes on this head",
					"repo", o.Repo, "pr", o.Number, "head", o.HeadSHA, "reason", outcome.Skipped)
			}
			continue
		}
		if logger != nil {
			logger.Info("transient CI rerun: re-ran failed jobs instead of a fix kick",
				"repo", o.Repo, "pr", o.Number, "head", o.HeadSHA, "runs", outcome.RunIDs)
		}
		if audit != nil {
			audit(o.Repo, o.Number, fmt.Sprintf("repo=%s, pr=%d, head=%s, runs=%d, reason=transient CI triage",
				o.Repo, o.Number, o.HeadSHA, len(outcome.RunIDs)))
		}
	}
	return attempted
}
