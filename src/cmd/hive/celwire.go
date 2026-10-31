package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

type celKickSender interface {
	SendKick(name, message string) error
}

// celTriggerSource identifies which forge object produced a CEL-evaluated event,
// used only for audit logging.
const (
	celSourceIssue = "issue"
	celSourcePR    = "pr"
)

// celEngineCache memoizes the compiled CEL trigger engine so it is recompiled
// only when the operator's `triggers:` list actually changes (config reload),
// not on every eval cycle. Compilation is fail-closed: a malformed rule leaves
// the previous engine in place (or no engine on first build), so existing
// label/governor behavior is never disturbed and the process never crashes.
type celEngineCache struct {
	mu     sync.Mutex
	sig    string             // signature of the triggers the engine was built from
	engine *celtrigger.Engine // nil when there are no rules or the last build failed
	built  bool               // whether an engine build has been attempted for sig
}

// globalCELEngine is the process-wide cache. runEvalCycle consults it each cycle;
// it rebuilds only on a triggers-list change.
var globalCELEngine celEngineCache

// triggersSignature returns a stable string identifying a triggers list, so the
// cache can detect a config-reload change without recompiling every cycle.
func triggersSignature(triggers []config.TriggerRule) string {
	if len(triggers) == 0 {
		return ""
	}
	var b []byte
	for _, t := range triggers {
		// Field separators (\x00 / \x1f) keep distinct rules from colliding.
		b = append(b, t.Name...)
		b = append(b, 0x1f)
		b = append(b, t.Expr...)
		b = append(b, 0x1f)
		b = append(b, t.Agent...)
		b = append(b, 0x1f)
		b = append(b, []byte(fmt.Sprintf("%d", t.Priority))...)
		b = append(b, 0x00)
	}
	return string(b)
}

// celEngineFor returns the compiled engine for the current config, rebuilding it
// only when cfg.Triggers changed since the last build. Returns nil when there are
// no rules (total no-op) or when the current rules failed to compile (fail-closed:
// existing behavior unchanged). Cheap and nil-safe: an empty triggers list does a
// signature compare and returns nil with no allocation of note.
func celEngineFor(cfg *config.Config, logger *slog.Logger) *celtrigger.Engine {
	if cfg == nil {
		return nil
	}
	triggers := cfg.TriggerRulesSnapshot()
	sig := triggersSignature(triggers)

	globalCELEngine.mu.Lock()
	defer globalCELEngine.mu.Unlock()

	// Unchanged since last build: reuse the cached result (engine or nil).
	if globalCELEngine.built && globalCELEngine.sig == sig {
		return globalCELEngine.engine
	}

	globalCELEngine.sig = sig
	globalCELEngine.built = true

	if len(triggers) == 0 {
		globalCELEngine.engine = nil
		return nil
	}

	engine, err := celtrigger.CompileFromConfig(&config.Config{Triggers: triggers})
	if err != nil {
		// Fail-closed: a malformed rule must never crash the fleet or silently
		// change routing. Log once (per triggers change) and run with no CEL
		// engine — existing label/governor triggering is exactly preserved.
		logger.Error("celtrigger: rejecting malformed trigger rules; CEL routing disabled until fixed",
			"error", err)
		globalCELEngine.engine = nil
		return nil
	}
	logger.Info("celtrigger: compiled trigger rules", "rules", engine.Len())
	globalCELEngine.engine = engine
	return engine
}

// issueToEvent maps an enumerated actionable issue onto a forge-neutral
// NormalizedEvent. The enumerator surfaces the current open snapshot of an issue,
// not the precise webhook transition that produced it, so Kind is set to
// KindIssueOpened for a newly-seen actionable issue.
//
// TODO(celtrigger transitions): the eval enumerator cannot distinguish
// opened/labeled/reopened transitions — it only knows "this issue is actionable
// now". Finer kinds (KindIssueLabeled, KindIssueReopened) require a real webhook
// event source; map them here once that path exists.
func issueToEvent(iss github.Issue) celtrigger.NormalizedEvent {
	return celtrigger.NormalizedEvent{
		Kind:      celtrigger.KindIssueOpened,
		Repo:      iss.Repo,
		Number:    iss.Number,
		Title:     iss.Title,
		Author:    iss.Author,
		Labels:    iss.Labels,
		Assignees: iss.Assignees,
		State:     "open",
	}
}

// prToEvent maps an enumerated actionable pull request onto a NormalizedEvent.
// As with issues, the enumerator gives the current snapshot rather than a
// transition, so Kind is KindPROpened. is_draft/base/head come from the PR when
// available.
//
// TODO(celtrigger transitions): finer PR kinds (pr.labeled, pr.ready_for_review,
// pr.merged) need a webhook event source; map them once that path exists.
func prToEvent(pr github.PullRequest) celtrigger.NormalizedEvent {
	return celtrigger.NormalizedEvent{
		Kind:    celtrigger.KindPROpened,
		Repo:    pr.Repo,
		Number:  pr.Number,
		Title:   pr.Title,
		Author:  pr.Author,
		Labels:  pr.Labels,
		IsDraft: pr.Draft,
		State:   "open",
	}
}

// celSeenSet remembers which repo#number items have already been surfaced as
// "opened", so issue.opened / pr.opened are edge-triggered per hive process
// instead of re-matching every eval cycle while the item stays actionable.
type celSeenSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// firstSight reports whether key is new, recording it as seen.
func (s *celSeenSet) firstSight(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]struct{})
	}
	if _, ok := s.seen[key]; ok {
		return false
	}
	s.seen[key] = struct{}{}
	return true
}

// globalCELSeen is the process-wide seen-set used by the eval cycle.
var globalCELSeen celSeenSet

// celMatchedAgents is the level-triggered form of celMatchedAgentsOnce: every
// enumerated item is evaluated every call.
func celMatchedAgents(
	engine *celtrigger.Engine,
	actionable *github.ActionableResult,
	cfg *config.Config,
	gov *governor.Governor,
	isPaused func(string) bool,
	logger *slog.Logger,
) []string {
	return celMatchedAgentsOnce(engine, actionable, cfg, gov, isPaused, logger, nil)
}

// celMatchedAgentsOnce returns the set of agent names that operator-defined CEL rules
// select for the items enumerated this cycle, after applying the governor's own
// gating. The result is intended to be UNIONED into the governor's due-agents set
// — it is strictly additive and never removes a governor-selected agent.
//
// Gating: an agent is only included if it is configured, enabled, not
// config-paused, not manager-paused, and passes governor.AgentEligibleForCELKick
// (cadence-pause / budget / on-demand / kick-channel). A CEL match therefore
// never bypasses a paused agent or an exhausted budget.
//
// When seen is non-nil, an issue or PR is evaluated only the first time it is
// enumerated; later cycles skip it. A nil seen evaluates every item every call.
//
// Cheap and nil-safe: a nil engine, nil actionable, or empty rules returns an
// empty result with zero per-item work.
func celMatchedAgentsOnce(
	engine *celtrigger.Engine,
	actionable *github.ActionableResult,
	cfg *config.Config,
	gov *governor.Governor,
	isPaused func(string) bool,
	logger *slog.Logger,
	seen *celSeenSet,
) []string {
	if engine == nil || engine.Len() == 0 || actionable == nil || cfg == nil {
		return nil
	}

	// Collect candidate agents keyed by name so an agent matched by multiple
	// items/rules is kicked once. Track a sample source for audit logging.
	type candidate struct {
		source string
		number int
	}
	candidates := make(map[string]candidate)

	consider := func(ev celtrigger.NormalizedEvent, source string) {
		for _, agentName := range engine.MatchAgents(ev) {
			if _, dup := candidates[agentName]; dup {
				continue
			}
			candidates[agentName] = candidate{source: source, number: ev.Number}
		}
	}

	for _, iss := range actionable.Issues.Items {
		if seen != nil && !seen.firstSight(fmt.Sprintf("issue:%s#%d", iss.Repo, iss.Number)) {
			continue
		}
		consider(issueToEvent(iss), celSourceIssue)
	}
	for _, pr := range actionable.PRs.Items {
		if seen != nil && !seen.firstSight(fmt.Sprintf("pr:%s#%d", pr.Repo, pr.Number)) {
			continue
		}
		consider(prToEvent(pr), celSourcePR)
	}

	if len(candidates) == 0 {
		return nil
	}

	matched := make([]string, 0, len(candidates))
	for agentName, c := range candidates {
		// Hard gate 1: the agent must exist, be enabled, and not be config-paused.
		ac, ok := cfg.Agents[agentName]
		if !ok {
			logger.Warn("celtrigger: rule references unknown agent; skipping",
				"agent", agentName)
			continue
		}
		if !ac.Enabled || ac.Paused {
			logger.Debug("celtrigger: matched agent is disabled or paused; skipping",
				"agent", agentName, "enabled", ac.Enabled, "paused", ac.Paused)
			continue
		}
		// Hard gate 2: manager-level (dashboard) pause is always respected.
		if isPaused != nil && isPaused(agentName) {
			logger.Debug("celtrigger: matched agent is paused (manager); skipping",
				"agent", agentName)
			continue
		}
		// Hard gate 3: governor gating (cadence-pause / budget / on-demand / kick).
		if gov != nil && !gov.AgentEligibleForCELKick(agentName) {
			logger.Debug("celtrigger: matched agent gated by governor; skipping",
				"agent", agentName)
			continue
		}

		logger.Info("celtrigger: rule matched — routing agent (additive)",
			"agent", agentName, "source", c.source, "number", c.number)
		matched = append(matched, agentName)
	}
	return matched
}

func celTriggerKickAgents(
	ctx context.Context,
	engine *celtrigger.Engine,
	event celtrigger.NormalizedEvent,
	cfg *config.Config,
	gov *governor.Governor,
	isPaused func(string) bool,
	kicker celKickSender,
	reason string,
	logger *slog.Logger,
) []string {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if engine == nil || engine.Len() == 0 || cfg == nil || kicker == nil {
		return nil
	}
	var kicked []string
	for _, agentName := range engine.MatchAgents(event) {
		select {
		case <-ctx.Done():
			logger.Warn("celtrigger: kick cancelled", "event", event.Kind, "error", ctx.Err())
			return kicked
		default:
		}
		ac, ok := cfg.Agents[agentName]
		if !ok {
			logger.Warn("celtrigger: rule references unknown agent; skipping",
				"agent", agentName, "event", event.Kind)
			continue
		}
		if !ac.Enabled || ac.Paused {
			logger.Debug("celtrigger: matched agent is disabled or paused; skipping",
				"agent", agentName, "enabled", ac.Enabled, "paused", ac.Paused, "event", event.Kind)
			continue
		}
		if isPaused != nil && isPaused(agentName) {
			logger.Debug("celtrigger: matched agent is paused (manager); skipping",
				"agent", agentName, "event", event.Kind)
			continue
		}
		if gov != nil && !gov.AgentEligibleForCELKick(agentName) {
			logger.Debug("celtrigger: matched agent gated by governor; skipping",
				"agent", agentName, "event", event.Kind)
			continue
		}
		if reason == "" {
			reason = fmt.Sprintf("celtrigger %s", event.Kind)
		}
		if err := kicker.SendKick(agentName, reason); err != nil {
			logger.Error("celtrigger: kick failed", "agent", agentName, "event", event.Kind, "error", err)
			continue
		}
		if gov != nil {
			gov.RecordKick(agentName)
		}
		kicked = append(kicked, agentName)
	}
	return kicked
}

// unionAgents appends any names from add that are not already in base, preserving
// base's order and appending new names in the given order. The result never drops
// a base entry — this is how CEL routing stays strictly additive over the
// governor's own selection.
func unionAgents(base, add []string) []string {
	if len(add) == 0 {
		return base
	}
	present := make(map[string]struct{}, len(base))
	for _, name := range base {
		agent, _ := config.SplitCadenceTargetKey(name)
		present[agent] = struct{}{}
	}
	for _, name := range add {
		if _, ok := present[name]; ok {
			continue
		}
		present[name] = struct{}{}
		base = append(base, name)
	}
	return base
}
