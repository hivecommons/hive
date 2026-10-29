package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// Outcomes of one ciFailureReactor.React call, logged with the signal.
const (
	ciFailureKicked           = "kicked"
	ciFailureAutomergePass    = "automerge_pass"
	ciFailureDisabled         = "disabled"
	ciFailureRepoPaused       = "repo_paused"
	ciFailureCooldown         = "cooldown"
	ciFailureBreaker          = "breaker_engaged"
	ciFailureAgentUnavailable = "agent_unavailable"
	ciFailureNoReceiver       = "no_receiver"
	ciFailureKickFailed       = "kick_failed"
)

// ciFailureReactor reacts to a CI failure signal a verified webhook recorded
// (hivecommons/hive#15291) within seconds instead of on the next sweep:
//
//   - a hive-authored PR (audit agent_pr_created attribution) gets a
//     targeted FIX-BEFORE-NEW kick to its owning agent, naming the failed
//     check and its log;
//   - any other PR (contributor or human) gets an automerge pass now, the
//     same pass that routes a red contributor PR back to its author
//     (stage 072) or to the rework lane.
//
// At most one reaction per PR head per governor.ci_failure_kick_cooldown_s.
// governor.ci_failure_webhook_kick=false, a paused repo, an engaged fleet
// breaker or an owner that cannot be kicked (paused, or paused in every
// governor mode) skips the reaction; the sweep and ci-failing.json still
// find the PR red as before.
type ciFailureReactor struct {
	mu   sync.Mutex
	last map[string]time.Time

	now       func() time.Time
	cfg       func() *config.Config
	owners    func(cfg *config.Config) map[string]string
	available func(cfg *config.Config) func(string) bool
	breaker   func() bool
	kicker    fixKicker
	nudge     func(repo string, number int)
	note      func(repo string, number int, agent, line string)
	logger    *slog.Logger
}

// React handles one signal and returns its outcome.
func (r *ciFailureReactor) React(sig github.CIFailureSignal) string {
	outcome, agent := r.react(sig)
	if r.logger != nil {
		r.logger.Info("ci failure webhook reaction", "repo", sig.Repo, "pr", sig.Number, "head_sha", sig.HeadSHA,
			"check", sig.Check, "outcome", outcome, "agent", agent)
	}
	return outcome
}

func (r *ciFailureReactor) react(sig github.CIFailureSignal) (string, string) {
	cfg := r.cfg()
	if !cfg.CIFailureWebhookKickEnabled() {
		return ciFailureDisabled, ""
	}
	if cfg.IsRepoPaused(sig.Repo) {
		return ciFailureRepoPaused, ""
	}
	key := escalation.Key(strings.ToLower(sig.Repo), sig.Number) + "@" + sig.HeadSHA
	if !r.reserve(key, cfg.CIFailureKickCooldown()) {
		return ciFailureCooldown, ""
	}
	if r.breaker != nil && r.breaker() {
		r.release(key)
		return ciFailureBreaker, ""
	}
	owner := ""
	if r.owners != nil {
		owner = ownerFor(r.owners(cfg), sig.Repo, sig.Number)
	}
	if owner == "" {
		if r.nudge == nil {
			r.release(key)
			return ciFailureNoReceiver, ""
		}
		r.nudge(sig.Repo, sig.Number)
		r.record(sig, "", fmt.Sprintf("CI failed on #%d (%s) - scheduled an automerge pass", sig.Number, sig.Check))
		return ciFailureAutomergePass, ""
	}
	if r.available != nil {
		if avail := r.available(cfg); avail != nil && !avail(owner) {
			r.release(key)
			return ciFailureAgentUnavailable, owner
		}
	}
	if r.kicker == nil {
		r.release(key)
		return ciFailureNoReceiver, owner
	}
	if err := r.kicker.Kick(owner, ciFailureFixKick(sig, owner)); err != nil {
		r.release(key)
		if r.logger != nil {
			r.logger.Warn("ci failure kick undeliverable", "repo", sig.Repo, "pr", sig.Number, "agent", owner, "error", err)
		}
		return ciFailureKickFailed, owner
	}
	r.record(sig, owner, fmt.Sprintf("CI failed on #%d (%s) - kicked agent %s", sig.Number, sig.Check, owner))
	return ciFailureKicked, owner
}

// reserve claims the cooldown slot for a PR head, so concurrent signals for
// two checks on the same head react once. Expired slots are dropped.
func (r *ciFailureReactor) reserve(key string, cooldown time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	for k, at := range r.last {
		if now.Sub(at) >= cooldown {
			delete(r.last, k)
		}
	}
	if _, held := r.last[key]; held {
		return false
	}
	r.last[key] = now
	return true
}

// release gives back a slot whose reaction did not happen, so a later
// signal for the same head can still react.
func (r *ciFailureReactor) release(key string) {
	r.mu.Lock()
	delete(r.last, key)
	r.mu.Unlock()
}

func (r *ciFailureReactor) record(sig github.CIFailureSignal, agent, line string) {
	if r.logger != nil {
		r.logger.Info(line, "repo", sig.Repo)
	}
	if r.note != nil {
		r.note(sig.Repo, sig.Number, agent, line)
	}
}

// ownerFor looks a PR up in an auditPRAgents map, whose keys spell the repo
// as the audit entry did; GitHub repository names are case-insensitive.
func ownerFor(owners map[string]string, repo string, number int) string {
	if a, ok := owners[escalation.Key(repo, number)]; ok {
		return a
	}
	want := escalation.Key(repo, number)
	for k, a := range owners {
		if strings.EqualFold(k, want) {
			return a
		}
	}
	return ""
}

// ciFailureFixKick is the targeted FIX-BEFORE-NEW kick for a webhook-reported
// failure: the same repair instructions the red-PR re-engagement kick
// carries, naming the failed check and pointing at its log.
func ciFailureFixKick(sig github.CIFailureSignal, owner string) string {
	o := escalation.Observation{
		Repo:          sig.Repo,
		Number:        sig.Number,
		HeadSHA:       sig.HeadSHA,
		Agent:         owner,
		FailingChecks: []string{sig.Check},
	}
	if sig.DetailsURL != "" {
		o.Excerpt = "log: " + sig.DetailsURL
	}
	msg := redPRFixKick(o, false)
	return strings.Replace(msg, "is red and has not moved", "just failed CI ("+sig.Check+")", 1)
}

// installCIFailureReactor builds the reactor the dashboard webhook receiver
// hands CI failure signals to. Every dependency is resolved per signal
// because this runs before the agent manager and dashboard server exist.
// Idempotent, like installWebhookWake.
func (b *boot) installCIFailureReactor() {
	if b.ciFailures != nil {
		return
	}
	b.ciFailures = &ciFailureReactor{
		now: time.Now,
		cfg: func() *config.Config { return b.cfg },
		owners: func(cfg *config.Config) map[string]string {
			return auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
		},
		available: func(cfg *config.Config) func(string) bool {
			if b.agentMgr == nil {
				return func(string) bool { return false }
			}
			return agentAvailability(cfg, b.agentMgr)
		},
		breaker: func() bool {
			if b.agentMgr == nil {
				return false
			}
			engaged, _ := b.agentMgr.BreakerState()
			return engaged
		},
		kicker: lazyAgentKicker{b: b},
		nudge:  b.sweepNudgeFunc(),
		note: func(repo string, number int, agent, line string) {
			if b.dashSrv != nil {
				b.dashSrv.AuditLogRecord("governor", "ci_failure_kick", line, agent, repo, number)
			}
		},
		logger: b.logger,
	}
}

// lazyAgentKicker resolves the agent manager at kick time.
type lazyAgentKicker struct{ b *boot }

func (k lazyAgentKicker) Kick(agent, message string) error {
	if k.b.agentMgr == nil {
		return fmt.Errorf("agent manager unavailable: cannot kick %s", agent)
	}
	return agentKicker{mgr: k.b.agentMgr}.Kick(agent, message)
}

// ciFailureFunc is the dashboard.Dependencies.CIFailure hook. The reaction
// (an audit read and possibly a kick) runs off the webhook handler's
// goroutine so GitHub gets its answer at once.
func (b *boot) ciFailureFunc() func(github.CIFailureSignal) {
	if b.ciFailures == nil {
		return nil
	}
	r := b.ciFailures
	return func(sig github.CIFailureSignal) { go r.React(sig) }
}
