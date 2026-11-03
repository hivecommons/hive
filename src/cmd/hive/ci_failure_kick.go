package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// ciFailureKickDefaultAgent is the agent an unattributed hive-authored PR is
// routed to, matching the fix-before-new builder's default (scanner is the
// fleet's primary PR creator).
const ciFailureKickDefaultAgent = "scanner"

// ciFailureKickSendTimeout bounds one targeted kick delivery; a stuck pane
// must not pin the worker.
const ciFailureKickSendTimeout = 2 * time.Minute

var errNoAgentRuntime = errors.New("agent runtime not available")

// ciFailureKicker turns a webhook CI-failure signal on a hive-authored PR
// into an immediate targeted kick of the owning agent. Gating mirrors the
// CEL trigger path (celTriggerKickAgents): config flag, agent enabled and
// not paused in config, runtime pause (which the fleet breaker also sets),
// governor eligibility (cadence pause, on-demand, budget), repo pause, and
// the per-PR-head cooldown kept by the signal store. The kick itself runs
// off the webhook goroutine so GitHub gets its 200 at once.
type ciFailureKicker struct {
	cfg       func() *config.Config
	gov       *governor.Governor
	isPaused  func(agent string) bool
	message   func(agent string, sig github.CIFailureSignal) string
	attribute func(repo string, number int) string
	send      func(ctx context.Context, agent, message string) error
	logger    *slog.Logger
	ctx       context.Context
	// spawn runs the kick delivery; tests swap in a synchronous runner.
	spawn func(func())
}

func (b *boot) newCIFailureKicker() *ciFailureKicker {
	k := &ciFailureKicker{
		cfg:       func() *config.Config { return b.cfg },
		gov:       b.gov,
		attribute: github.AuthorAgentForPR,
		logger:    b.logger,
		ctx:       b.ctx,
		spawn:     func(fn func()) { go fn() },
	}
	// Resolve the scheduler and runtime per call: the dependencies closure
	// may be built before boot finishes wiring them.
	k.message = func(agent string, sig github.CIFailureSignal) string {
		if b.sched == nil {
			return ""
		}
		return b.sched.CIFailureKickMessage(agent, sig)
	}
	k.isPaused = func(agent string) bool { return b.agentMgr != nil && b.agentMgr.IsPaused(agent) }
	k.send = func(_ context.Context, agent, message string) error {
		if b.agentMgr == nil {
			return errNoAgentRuntime
		}
		return b.agentMgr.SendKick(agent, message)
	}
	return k
}

// ciFailureKickFunc is the dashboard.Dependencies.CIFailureKick hook.
func (b *boot) ciFailureKickFunc() func(sig github.CIFailureSignal) dashboard.CIFailureKickOutcome {
	k := b.newCIFailureKicker()
	return k.React
}

// React decides and, when every gate passes, dispatches the kick.
func (k *ciFailureKicker) React(sig github.CIFailureSignal) dashboard.CIFailureKickOutcome {
	if k == nil {
		return dashboard.CIFailureKickOutcome{Reason: "no kicker"}
	}
	logger := k.logger
	if logger == nil {
		logger = slog.Default()
	}
	cfg := k.cfg()
	if cfg == nil {
		return dashboard.CIFailureKickOutcome{Reason: "no config"}
	}
	if !cfg.CIFailureWebhookKick() {
		return dashboard.CIFailureKickOutcome{Reason: "disabled"}
	}
	agent := ""
	if k.attribute != nil {
		agent = strings.TrimSpace(k.attribute(sig.Repo, sig.Number))
	}
	if agent == "" {
		agent = ciFailureKickDefaultAgent
	}
	out := dashboard.CIFailureKickOutcome{Agent: agent}
	ac, ok := cfg.Agents[agent]
	if !ok {
		out.Reason = "unknown agent"
		return out
	}
	if !ac.Enabled || ac.Paused {
		out.Reason = "agent paused"
		return out
	}
	if k.isPaused != nil && k.isPaused(agent) {
		out.Reason = "agent paused"
		return out
	}
	if cfg.IsRepoPaused(sig.Repo) {
		out.Reason = "repo paused"
		return out
	}
	if k.gov != nil && !k.gov.AgentEligibleForCELKick(agent) {
		out.Reason = "governor gated"
		return out
	}
	// Attribute before rendering so the fix-before-new builder lists the
	// signal under this agent.
	github.CIFailureSignals().SetAgent(sig.Repo, sig.Number, agent)
	sig.Agent = agent
	message := ""
	if k.message != nil {
		message = k.message(agent, sig)
	}
	if strings.TrimSpace(message) == "" {
		out.Reason = "nothing to repair"
		return out
	}
	if !github.CIFailureSignals().AllowKick(sig.Repo, sig.Number, sig.HeadSHA, cfg.CIFailureKickCooldown()) {
		out.Reason = "cooldown"
		return out
	}
	if k.send == nil {
		out.Reason = "no kick sender"
		return out
	}
	out.Kicked = true
	spawn := k.spawn
	if spawn == nil {
		spawn = func(fn func()) { go fn() }
	}
	spawn(func() {
		parent := k.ctx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, ciFailureKickSendTimeout)
		defer cancel()
		if err := k.send(ctx, agent, message); err != nil {
			logger.Warn("ci failure webhook kick failed", "agent", agent, "repo", sig.Repo, "number", sig.Number, "check", sig.Check, "error", err)
			return
		}
		if k.gov != nil {
			k.gov.RecordKickForRepo(agent, sig.Repo)
		}
		logger.Info("ci failure webhook kick delivered", "agent", agent, "repo", sig.Repo, "number", sig.Number, "check", sig.Check)
	})
	return out
}
