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

// rerunTransientTriagedPRs is the fix loop's answer to a red hive-authored or
// trusted-bot PR (auto_merge.trusted_bot_authors, dbrTransientRerunAuthor)
// whose every failing check run an agent triaged as infrastructure, not this
// change (`hive-ci-transient` markers, github.PullRequest.TransientCITriaged):
// it re-runs the failed jobs once per head through rerun-failed-jobs, and
// never kicks a fixer or pushes a commit for it. A trusted-bot PR's red-CI
// hand-off waits for this re-run and reads its outcome from the ledger
// (dependency_bot_transient_rerun.go). On
// projectbluefin/common#1647 the fix loop instead kicked the owning agent six
// times on one head, each kick answered with "infrastructure, not this
// change", escalated the PR to a person, and a lane then pushed an empty
// "retrigger CI" commit.
//
// It runs after recordRedStaleness (which records the triaged head in the
// ledger) and before reapStuckRedPRs, whose kicks deliverReEngagement holds
// while a re-run is owed. audit, when non-nil, receives one line per PR the
// hive re-ran.
//
// It returns the escalation keys of the PRs whose failed jobs it re-ran this
// pass. The reaper runs next in the same cycle, on the same snapshot, where
// those PRs still read red and stale, and the re-run is no longer owed; the
// caller skips them there (reaperSkipAfterRerun) so a re-run and a fix kick
// never go out together for one head (hivecommons/hive#17002 review). A PR
// whose re-run was skipped (no failed runs, re-run forbidden) is not
// returned: the fix loop resumes on it, as intended. A trusted-bot PR is
// returned too when re-run; the reaper only walks hive-agent PRs, so its
// key in the skip set changes nothing.
func rerunTransientTriagedPRs(
	ctx context.Context,
	cfg *config.Config,
	rerunner transientCIRerunner,
	actionable *github.ActionableResult,
	escalatedPRs map[string]bool,
	audit func(repo string, number int, detail string),
	logger *slog.Logger,
) map[string]bool {
	if cfg == nil || cfg.Escalation.Disabled || rerunner == nil || actionable == nil {
		return nil
	}
	store := getEscalationStore()
	attempted := 0
	var reran map[string]bool
	for _, pr := range actionable.PRs.Items {
		if attempted >= transientCIRerunMaxPerPass {
			break
		}
		if !dbrTransientRerunAuthor(cfg, pr.Author) || !pr.TransientCITriaged || pr.CIChecksRunning || !pr.HasFailingRequiredCheck() {
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
		if len(outcome.RunIDs) == 0 {
			// Kept with the spent head so a dependency-bot hand-off can
			// say the re-run was not allowed (or found nothing to re-run).
			skipped := outcome.Skipped
			if skipped == "" {
				skipped = "rerun-failed"
			}
			store.RecordTransientRerunOutcome(o.Repo, o.Number, o.HeadSHA, skipped)
			if logger != nil {
				logger.Info("transient CI rerun skipped; the fix loop (or, for a dependency-bot PR, the red-CI hand-off) resumes on this head",
					"repo", o.Repo, "pr", o.Number, "head", o.HeadSHA, "reason", skipped)
			}
			continue
		}
		store.RecordTransientRerun(o.Repo, o.Number, o.HeadSHA)
		if logger != nil {
			logger.Info("transient CI rerun: re-ran failed jobs instead of a fix kick",
				"repo", o.Repo, "pr", o.Number, "head", o.HeadSHA, "runs", outcome.RunIDs)
		}
		if audit != nil {
			audit(o.Repo, o.Number, fmt.Sprintf("repo=%s, pr=%d, head=%s, runs=%d, reason=transient CI triage",
				o.Repo, o.Number, o.HeadSHA, len(outcome.RunIDs)))
		}
		if reran == nil {
			reran = map[string]bool{}
		}
		reran[key] = true
	}
	return reran
}

// reaperSkipAfterRerun is the set the reaper skips this cycle: the escalated
// PRs, plus the PRs whose failed jobs rerunTransientTriagedPRs just re-ran.
// escalated is returned unchanged when nothing was re-ran.
func reaperSkipAfterRerun(escalated, reran map[string]bool) map[string]bool {
	if len(reran) == 0 {
		return escalated
	}
	skip := make(map[string]bool, len(escalated)+len(reran))
	for k, v := range escalated {
		skip[k] = v
	}
	for k := range reran {
		skip[k] = true
	}
	return skip
}
