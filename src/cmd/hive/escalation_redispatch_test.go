package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/timeline"
)

// staleRedActionable ages a red PR past RedPRStaleAfter so the reaper sees it
// as stuck rather than churning.
func staleRedActionable(t *testing.T, cfg *config.Config, clock *time.Time) *github.ActionableResult {
	t.Helper()
	actionable := actionableWith(redPR("widgets", 7, "hive-agent", "abc"))
	recordRedStaleness(cfg, actionable)
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
	return actionable
}

// The reaper must DELIVER a fix kick, not just increment a counter. Before
// hivecommons/hive#9472 the only thing that reached the owner was its next
// cadence kick, so a PR could be escalated as "6 automated fix re-dispatches"
// having never been mentioned to any agent.
func TestReapStuckRedPRs_DeliversKickToOwner(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	fix := staleRedActionable(t, cfg, clock)
	kicker := &fakeKicker{}

	reapStuckRedPRs(cfg, fix, map[string]bool{}, kicker, nil, discardLogger())

	if len(kicker.kicks) != 1 {
		t.Fatalf("stale red PR must receive exactly one targeted kick, got %d", len(kicker.kicks))
	}
	if kicker.kicks[0].agent != "scanner" {
		t.Errorf("kick went to %q, want the unattributed default owner scanner", kicker.kicks[0].agent)
	}
	for _, want := range []string{"FIX-BEFORE-NEW", "acme/widgets", "#7", "abc"} {
		if !strings.Contains(kicker.kicks[0].message, want) {
			t.Errorf("kick message missing %q:\n%s", want, kicker.kicks[0].message)
		}
	}
	if got := store.ReEngagements("acme/widgets", 7); got != 1 {
		t.Errorf("a delivered kick must charge exactly one re-engagement, got %d", got)
	}
}

// An undeliverable kick (agent not running, provider backoff, restart) must not
// spend the PR's budget: the budget exists to bound attempts an agent received.
func TestReapStuckRedPRs_UndeliverableKickChargesNoBudget(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	fix := staleRedActionable(t, cfg, clock)
	kicker := &fakeKicker{err: errors.New("agent not running")}

	for i := 0; i < escalation.MaxReEngagements+3; i++ {
		reapStuckRedPRs(cfg, fix, map[string]bool{}, kicker, nil, discardLogger())
		*clock = clock.Add(escalation.ReEngageCooldown + time.Minute)
	}

	if got := store.ReEngagements("acme/widgets", 7); got != 0 {
		t.Fatalf("undeliverable kicks charged %d re-engagements, want 0 — budget must never be spent on kicks nobody got", got)
	}
}

// A paused owner can never answer a FIX-BEFORE-NEW kick (the v5 L6 pack pauses
// strategist in every mode), so the repair is routed to the fallback fixer
// rather than burning the PR's budget on nobody.
func TestReapStuckRedPRs_ReroutesUnavailableOwner(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	cfg := escalationTestConfig()
	cfg.Review.FixerAgent = "fixer"
	fix := staleRedActionable(t, cfg, clock)
	kicker := &fakeKicker{}
	available := func(name string) bool { return name != "scanner" }

	reapStuckRedPRs(cfg, fix, map[string]bool{}, kicker, available, discardLogger())

	if len(kicker.kicks) != 1 || kicker.kicks[0].agent != "fixer" {
		t.Fatalf("an unreachable owner's PR must be rerouted to review.fixer_agent, got %+v", kicker.kicks)
	}
	if got := store.ReEngagements("acme/widgets", 7); got != 1 {
		t.Errorf("a delivered rerouted kick must charge one re-engagement, got %d", got)
	}

	// Nobody reachable at all: no kick, no budget spent, so the PR is never
	// escalated as having had attempts it never got.
	*clock = clock.Add(escalation.MaxReEngageCooldown + time.Minute)
	none := func(string) bool { return false }
	reapStuckRedPRs(cfg, fix, map[string]bool{}, kicker, none, discardLogger())
	if len(kicker.kicks) != 1 {
		t.Errorf("no reachable fixer must mean no kick, got %d kicks", len(kicker.kicks))
	}
	if got := store.ReEngagements("acme/widgets", 7); got != 1 {
		t.Errorf("no reachable fixer must not charge budget, got %d", got)
	}
}

// The budget must not be spendable faster than the owner's cadence: on a 1h
// cadence, six 10-minute-spaced grants all fall inside one kick window.
func TestOwnerReEngageCooldownFollowsSlowestCadence(t *testing.T) {
	cfg := escalationTestConfig()
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": "1h"}},
		"busy": {Cadences: map[string]config.Cadence{"scanner": "20m"}},
	}
	if got := ownerReEngageCooldown(cfg, "scanner"); got != time.Hour {
		t.Fatalf("ownerReEngageCooldown = %s, want the slowest configured cadence 1h", got)
	}
	if got := escalation.EffectiveReEngageCooldown(time.Hour); got != time.Hour {
		t.Fatalf("effective cooldown = %s, want 1h", got)
	}
}

// An agent paused in every mode is not a candidate fixer.
func TestAgentAvailabilityHonoursPausedCadences(t *testing.T) {
	cfg := escalationTestConfig()
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"strategist": "paused", "scanner": "15m"}},
		"busy": {Cadences: map[string]config.Cadence{"strategist": "paused", "scanner": "5m"}},
	}
	available := agentAvailability(cfg, nil)
	if available("strategist") {
		t.Error("an agent paused in every mode must not be treated as reachable")
	}
	if !available("scanner") {
		t.Error("a cadenced agent must be reachable")
	}
	if !available("unconfigured") {
		t.Error("an agent with no cadence entry must stay reachable — that is a fleet-config signal, not a disablement")
	}
}

// The escalation_red hook payload must carry the PR's owning agent: the
// catalog and hooks.md have always documented `agent`, but nothing set it, so
// `when: t.agent == "scanner"` could never match (hivecommons/hive#9474).
func TestRecordRedStalenessFiresEscalationRedHookWithAgent(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })
	newTestEscalationStore(t)

	store := timeline.NewStore()
	audit := &hookWireAudit{ch: make(chan string, 8)}
	hookCfg := &config.Config{Hooks: []config.HookRule{{
		Name:   "route-red-pr",
		On:     "escalation_red",
		Action: "annotate",
		Params: map[string]string{"note": "red pr owned by scanner", "issue_ref": "escalation"},
		When:   `t.agent == "scanner" && t.attrs.pr == "7"`,
	}}}
	buildHookDispatcher(hookCfg, hookSinks{Timeline: store, Audit: audit}, hookTestLogger())

	recordRedStaleness(escalationTestConfig(), actionableWith(redPR("widgets", 7, "hive-agent", "abc")))

	deadline := time.After(2 * time.Second)
	for audit.count(hooks.AuditHookFired) == 0 {
		select {
		case <-audit.ch:
		case <-deadline:
			t.Fatalf("escalation_red hook keyed on t.agent never fired; audit=%v", audit.snapshot())
		}
	}
	hookDispatcher().Wait()

	if events := store.Recent(10); len(events) != 1 {
		t.Fatalf("expected the hook's annotation, got %d events", len(events))
	}
}
