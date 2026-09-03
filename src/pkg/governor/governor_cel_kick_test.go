package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// AgentEligibleForCELKick (added by the celtrigger port, #3608) is the gate a
// CEL-selected agent must pass before an event-driven kick. CEL triggering is
// strictly ADDITIVE: it may only ever add an agent the governor's own gates
// already allow. These tests pin each of those gates, because a regression here
// would let a declarative rule bypass a pause or an exhausted budget — the one
// thing the design promises it cannot do.

// TestAgentEligibleForCELKick_AllowsNormalAgent is the positive control: with
// no gate engaged, an ordinary agent is eligible. Without this, every negative
// assertion below could pass for the wrong reason (a function that always
// returns false would satisfy them all).
func TestAgentEligibleForCELKick_AllowsNormalAgent(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	if !g.AgentEligibleForCELKick("scanner") {
		t.Error("an unpaused, in-budget, kick-channel agent should be CEL-eligible")
	}
}

// TestAgentEligibleForCELKick_DeniedWhenCadencePaused: a paused agent must never
// be kicked, no matter what event fired. A CEL rule is not an override.
func TestAgentEligibleForCELKick_DeniedWhenCadencePaused(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.mu.Lock()
	cadence := g.state.Cadences["scanner"]
	cadence.Paused = true
	g.state.Cadences["scanner"] = cadence
	g.mu.Unlock()

	if g.AgentEligibleForCELKick("scanner") {
		t.Error("a paused agent must not be CEL-eligible")
	}
}

// TestAgentEligibleForCELKick_DeniedForOnDemand: on-demand agents are triggered
// only explicitly, never by the governor and never by an event rule.
func TestAgentEligibleForCELKick_DeniedForOnDemand(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	agents["scanner"] = config.AgentConfig{Enabled: true, OnDemand: true}
	g := New(cfg, agents, testLogger())

	if g.AgentEligibleForCELKick("scanner") {
		t.Error("an on-demand agent must not be CEL-eligible")
	}
}

// TestAgentEligibleForCELKick_DeniedWithoutKickChannel: an agent that does not
// subscribe to the kick channel is not governor-kickable, so CEL cannot kick it
// either.
func TestAgentEligibleForCELKick_DeniedWithoutKickChannel(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	agents["scanner"] = config.AgentConfig{
		Enabled:  true,
		Channels: []config.ChannelConfig{{Type: "review"}},
	}
	g := New(cfg, agents, testLogger())

	if g.AgentEligibleForCELKick("scanner") {
		t.Error("an agent without the kick channel must not be CEL-eligible")
	}
}

// TestAgentEligibleForCELKick_DeniedWhenBudgetExhausted: the weekly budget is a
// hard stop. This is the gate whose bypass would be most expensive.
func TestAgentEligibleForCELKick_DeniedWhenBudgetExhausted(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now())

	if g.AgentEligibleForCELKick("scanner") {
		t.Error("an exhausted budget must make an agent CEL-ineligible")
	}
}

// TestAgentEligibleForCELKick_BudgetExemptStillEligible mirrors agentsDueForKick:
// budget-exempt agents keep running even when the budget is exhausted.
func TestAgentEligibleForCELKick_BudgetExemptStillEligible(t *testing.T) {
	cfg, agents := standardConfig("scanner", "outreach")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now())
	g.SetBudgetIgnored([]string{"outreach"})

	if !g.AgentEligibleForCELKick("outreach") {
		t.Error("a budget-exempt agent should stay CEL-eligible when the budget is exhausted")
	}
	if g.AgentEligibleForCELKick("scanner") {
		t.Error("a non-exempt agent must stay ineligible when the budget is exhausted")
	}
}

// TestAgentEligibleForCELKick_UnknownAgent: an agent absent from the config map
// has no OnDemand/channel entry to consult. It is not gated by those checks, so
// this pins the documented behaviour rather than leaving it to chance.
func TestAgentEligibleForCELKick_UnknownAgent(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	if !g.AgentEligibleForCELKick("no-such-agent") {
		t.Error("an unknown agent hits no gate and should be reported eligible")
	}
}
