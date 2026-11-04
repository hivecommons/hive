package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	gov       func() *governor.Governor
	isPaused  func(agent string) bool
	message   func(agent string, sig github.CIFailureSignal) string
	attribute func(repo string, number int) string
	send      func(ctx context.Context, agent, message string) error
	logger    *slog.Logger
	ctx       context.Context
	// spawn runs the kick delivery; tests swap in a synchronous runner.
	spawn func(func())

	mu      sync.Mutex
	pending map[ciFailurePendingKey]*ciFailurePending
	now     func() time.Time
	wait    func(context.Context, time.Duration) bool
}

func (b *boot) newCIFailureKicker() *ciFailureKicker {
	k := &ciFailureKicker{
		cfg:       func() *config.Config { return b.cfg },
		gov:       func() *governor.Governor { return b.gov },
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
	b.ciFailureKicker = b.newCIFailureKicker()
	return b.ciFailureKicker.React
}

// React decides and, when every gate passes, dispatches the kick.
func (k *ciFailureKicker) React(sig github.CIFailureSignal) dashboard.CIFailureKickOutcome {
	if k == nil {
		return dashboard.CIFailureKickOutcome{Reason: "no kicker"}
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
	if reason := k.gate(sig, agent); reason != "" {
		out.Reason = reason
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
	if k.send == nil {
		out.Reason = "no kick sender"
		return out
	}
	p, reason := k.reserve(sig, message)
	if reason != "" {
		out.Reason = reason
		return out
	}
	if !github.CIFailureSignals().AllowKick(sig.Repo, sig.Number, sig.HeadSHA, cfg.CIFailureKickCooldown()) {
		k.remove(p)
		out.Reason = "cooldown"
		return out
	}
	out.Kicked = true
	k.dispatch(p, ciFailureKickImmediateAttempts)
	return out
}

// gate is rechecked before deferred delivery, so a queued kick never bypasses
// a pause, config change or governor budget gate that landed during backoff.
func (k *ciFailureKicker) gate(sig github.CIFailureSignal, agent string) string {
	cfg := k.cfg()
	if cfg == nil {
		return "no config"
	}
	if !cfg.CIFailureWebhookKick() {
		return "disabled"
	}
	ac, ok := cfg.Agents[agent]
	if !ok {
		return "unknown agent"
	}
	if !ac.Enabled || ac.Paused || (k.isPaused != nil && k.isPaused(agent)) {
		return "agent paused"
	}
	if cfg.IsRepoPaused(sig.Repo) {
		return "repo paused"
	}
	if gov := k.governor(); gov != nil && !gov.AgentEligibleForCELKick(agent) {
		return "governor gated"
	}
	return ""
}

func (k *ciFailureKicker) governor() *governor.Governor {
	if k.gov != nil {
		return k.gov()
	}
	return nil
}
