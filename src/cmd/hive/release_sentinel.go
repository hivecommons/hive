package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/releasesentinel"
)

// releaseSentinelStatePath is the sentinel's durable per-release state. It
// lives on the PVC (/data) so a restart resumes a release mid-repair with its
// round count intact; a var so tests can redirect it.
var releaseSentinelStatePath = "/data/release-sentinel.json"

// releaseSentinelLastRun is the wall-clock of the last sentinel pass (zero =
// never). Package-level because the eval tick is a free function; only the
// eval goroutine touches it.
var releaseSentinelLastRun time.Time

// runReleaseSentinel is the eval-tick half of the opt-in release sentinel
// (hivecommons/hive#9585). With release_sentinel.enabled off (the default)
// or HIVE_RELEASE_SENTINEL_ENABLED=false it returns before touching GitHub
// or the disk. When on, it runs one state-machine pass at most once per
// poll interval: repair rounds go out as a targeted kick through the same
// agent-kick path the stuck-PR reaper uses, and escalations go out through
// the hive's notification channels. It never pushes and never moves a tag.
func runReleaseSentinel(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	kicker fixKicker,
	available func(string) bool,
	notifier *notify.Notifier,
	logger *slog.Logger,
) {
	if cfg == nil || !cfg.ReleaseSentinelEnabled() {
		return
	}
	gh := ghClient.GoGitHub()
	if gh == nil {
		return
	}
	now := time.Now()
	interval := cfg.ReleaseSentinel.PollIntervalDuration()
	if interval <= 0 {
		interval = releasesentinel.DefaultPollInterval
	}
	if !releaseSentinelLastRun.IsZero() && now.Sub(releaseSentinelLastRun) < interval {
		return
	}
	releaseSentinelLastRun = now

	owner, repo, ok := releasesentinel.SplitRepo(cfg.ReleaseSentinelRepo())
	if !ok {
		logger.Warn("release sentinel enabled but no owner/repo to watch; set release_sentinel.repo or project.primary_repo",
			"repo", cfg.ReleaseSentinelRepo())
		return
	}
	full := owner + "/" + repo
	s := releasesentinel.New(
		releasesentinel.Options{
			Repo:            full,
			Agent:           cfg.ReleaseSentinel.Agent,
			MaxRounds:       cfg.ReleaseSentinel.MaxRounds,
			RoundTimeout:    cfg.ReleaseSentinel.RoundTimeoutDuration(),
			IgnoreWorkflows: cfg.ReleaseSentinel.IgnoreWorkflows,
		},
		releasesentinel.NewGitHubSource(gh, owner, repo),
		releasesentinel.NewFileStore(releaseSentinelStatePath),
		releaseSentinelDispatcher{kicker: kicker, available: available},
		releaseSentinelEscalator{notifier: notifier, logger: logger},
	)
	res, err := s.Evaluate(ctx)
	if err != nil {
		logger.Warn("release sentinel pass failed", "repo", full, "error", err)
		return
	}
	if res.Record == nil {
		return
	}
	attrs := []any{
		"repo", full, "tag", res.Record.Tag, "sha", res.Record.SHA,
		"state", res.Record.State, "round", res.Record.Round,
		"action", res.Action, "stale_runs", res.StaleRuns,
	}
	if len(res.Superseded) > 0 {
		attrs = append(attrs, "superseded", res.Superseded)
	}
	switch res.Action {
	case releasesentinel.ActionRoundStarted, releasesentinel.ActionEscalated, releasesentinel.ActionGreen:
		logger.Info("release sentinel transition", attrs...)
	default:
		logger.Debug("release sentinel pass", attrs...)
	}
}

// releaseSentinelDispatcher delivers a repair round as a targeted kick. An
// unreachable agent is a delivery failure, so the round is not counted and
// the sentinel retries on its next pass.
type releaseSentinelDispatcher struct {
	kicker    fixKicker
	available func(string) bool
}

func (d releaseSentinelDispatcher) DispatchRepair(_ context.Context, req releasesentinel.RepairRequest) error {
	if d.kicker == nil {
		return errors.New("no agent kicker")
	}
	if d.available != nil && !d.available(req.Agent) {
		return fmt.Errorf("agent %s is paused or has no reachable cadence", req.Agent)
	}
	// The evidence is CI output an agent may quote into an issue or PR, so
	// it goes through the same mention sanitizer as everything else the hive
	// can end up posting.
	return d.kicker.Kick(req.Agent, advisory.NeutralizeMentions(releasesentinel.RenderRepairKick(req)))
}

// releaseSentinelEscalator tells a human through the hive's notification
// channels and always leaves a warning in the log.
type releaseSentinelEscalator struct {
	notifier *notify.Notifier
	logger   *slog.Logger
}

func (e releaseSentinelEscalator) Escalate(_ context.Context, esc releasesentinel.Escalation) {
	if e.logger != nil {
		e.logger.Warn("release sentinel escalated a release to a human",
			"repo", esc.Repo, "tag", esc.Tag, "sha", esc.SHA, "reason", esc.Reason, "round", esc.Round, "detail", esc.Detail)
	}
	if e.notifier == nil {
		return
	}
	title, body := releasesentinel.RenderEscalation(esc)
	e.notifier.Send(advisory.NeutralizeMentions(title), advisory.NeutralizeMentions(body), notify.PriorityHigh)
}
