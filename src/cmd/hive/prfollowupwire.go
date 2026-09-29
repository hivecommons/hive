package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prfollowup"
)

// PR follow-up session resume wiring (hivecommons/hive#9583). Both halves are
// no-ops unless turn.pr_follow_up.enabled (or HIVE_PR_FOLLOWUP_RESUME) is on:
//
//   - recordPRFollowUpPointer runs from the PR-request watcher's PR-opened
//     hook and saves (repo, PR) -> authoring agent + its live CLI session.
//   - routePRFollowUps runs on the eval tick and feeds CI failures,
//     changes-requested reviews and new review-bot threads on those PRs back
//     into the authoring session. Anything it cannot resume is left to the
//     existing fix-before-new path (ci-failing.json / review-threads.json),
//     which is unchanged.

// prFollowUpSessions is the slice of *agent.Manager the PR-opened hook needs.
type prFollowUpSessions interface {
	SessionID(name string) (string, bool)
}

// recordPRFollowUpPointer saves the pointer for a PR the watcher just opened
// for agentName. Failures are logged, never fatal: without a pointer the PR
// simply keeps today's fresh-dispatch follow-up path.
func recordPRFollowUpPointer(cfg *config.Config, sessions prFollowUpSessions, agentName, repo string, number int, url string, now time.Time, logger *slog.Logger) {
	if !cfg.PRFollowUpResumeEnabled() {
		return
	}
	session := ""
	if sessions != nil {
		session, _ = sessions.SessionID(agentName)
	}
	if err := prfollowup.Record(context.Background(), prfollowup.Dir(), agentName, repo, number, url, session, now); err != nil && logger != nil {
		logger.Warn("failed to record PR follow-up pointer", "agent", agentName, "repo", repo, "pr", number, "error", err)
	}
}

// prFollowUpResumer adapts *agent.Manager to prfollowup.Resumer, translating
// the manager's transient refusal into prfollowup.ErrBusy.
type prFollowUpResumer struct{ mgr *agent.Manager }

func (r prFollowUpResumer) SessionID(name string) (string, bool) {
	return r.mgr.SessionID(name)
}

func (r prFollowUpResumer) SendResumeKick(name, message, sessionID string) error {
	err := r.mgr.SendResumeKick(name, message, sessionID)
	if errors.Is(err, agent.ErrResumeBusy) {
		return fmt.Errorf("%w: %v", prfollowup.ErrBusy, err)
	}
	return err
}

// routePRFollowUps is the eval-tick half. It reads only what the tick already
// has (the enumerated PRs, the escalation verdicts, review-threads.json) and
// makes no GitHub calls.
func routePRFollowUps(ctx context.Context, cfg *config.Config, actionable *github.ActionableResult, escalatedPRs map[string]bool, agentMgr *agent.Manager, logger *slog.Logger) []prfollowup.Outcome {
	if actionable == nil || !cfg.PRFollowUpResumeEnabled() {
		return nil
	}
	var resumer prfollowup.Resumer
	if agentMgr != nil {
		resumer = prFollowUpResumer{mgr: agentMgr}
	}
	opts := prfollowup.Options{
		Dir:    prfollowup.Dir(),
		MaxAge: cfg.PRFollowUpMaxAge(),
		Skip: func(repo string, number int) bool {
			return escalatedPRs[escalation.Key(repo, number)]
		},
		Logger: logger,
	}
	if report, err := github.ReadReviewThreadsReport(""); err == nil {
		opts.Threads = prfollowup.ThreadsFromReport(report)
	}
	return prfollowup.Route(ctx, actionable.PRs.Items, resumer, opts, time.Now())
}
