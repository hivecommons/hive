package hub

import (
	"strings"
	"testing"
	"time"
)

// settled is a start time far enough in the past that inactiveAgentStartupGrace
// has passed, so an agent's signals are eligible for evaluation.
func settled(now time.Time) string {
	return now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
}

// activeAt formats a last-activity timestamp d ago.
func activeAt(now time.Time, d time.Duration) string {
	return now.Add(-d).UTC().Format(time.RFC3339)
}

// TestPausedAgentIsNeverInactive is the rule that matters most: on a real
// hosted hive four agents (architect, outreach, strategist, supervisor) sit
// deliberately paused with trigger "dashboard-api". Reporting an operator's own
// choice as a fault is exactly the false positive that makes a facet useless.
func TestPausedAgentIsNeverInactive(t *testing.T) {
	now := time.Now()
	// Every fault signal at once — and it must STILL be silent, because paused
	// wins over all of them.
	cases := []AgentSummary{
		{Name: "architect", State: "paused", Paused: true},
		// Parked before Start() ran, so State still reads "running" and only
		// the Paused bool marks the operator's choice. A paused agent has no
		// live tmux session BY DESIGN, so this case also carries
		// SessionMissing — without the Paused check it would be reported as a
		// zombie, which is the exact false alarm this facet must never raise.
		{Name: "outreach", State: "running", Paused: true, SessionMissing: true, StartedAt: settled(now)},
		// Paused, session gone, unauthenticated, idle forever.
		{
			Name: "strategist", State: "paused", Paused: true,
			SessionMissing: true, NeedsLogin: true,
			StartedAt: settled(now), LastActivityAt: activeAt(now, 30*24*time.Hour),
		},
		// State says paused even though the bool was not set.
		{Name: "supervisor", State: "paused"},
	}
	for _, a := range cases {
		if got := classifyInactiveAgent(a, 500, now); got != agentInactiveNone {
			t.Errorf("paused agent %q classified as %v; paused must never alarm", a.Name, got.label())
		}
	}

	rep := evaluateInactiveAgents(cases, 500, now)
	if rep.Count != 0 {
		t.Fatalf("report counted %d paused agents (%q); want 0", rep.Count, rep.Reason)
	}
}

func TestSessionMissingIsReported(t *testing.T) {
	now := time.Now()
	a := AgentSummary{Name: "scanner", State: "running", SessionMissing: true, StartedAt: settled(now)}
	// A zombie is a fault even with an empty queue — there is no work to do,
	// but the agent is also not there to do any.
	if got := classifyInactiveAgent(a, 0, now); got != agentInactiveSessionMissing {
		t.Fatalf("got %v, want session-missing", got.label())
	}
}

func TestNeedsLoginReportedOnlyAfterGrace(t *testing.T) {
	now := time.Now()
	base := AgentSummary{Name: "quality", State: "running", NeedsLogin: true}

	// Within the login grace (but past startup grace): still settling.
	within := base
	within.StartedAt = now.Add(-(inactiveAgentStartupGrace + time.Minute)).UTC().Format(time.RFC3339)
	if got := classifyInactiveAgent(within, 0, now); got != agentInactiveNone {
		t.Errorf("agent inside login grace classified %v; want none", got.label())
	}

	// Past the grace: a genuinely unauthenticated agent.
	past := base
	past.StartedAt = now.Add(-(inactiveAgentNeedsLoginGrace + time.Minute)).UTC().Format(time.RFC3339)
	if got := classifyInactiveAgent(past, 0, now); got != agentInactiveNeedsLogin {
		t.Errorf("got %v, want needs-login", got.label())
	}

	// No start time at all: unknown uptime is not evidence of a wedged login.
	unknown := base
	if got := classifyInactiveAgent(unknown, 0, now); got != agentInactiveNone {
		t.Errorf("agent with unknown start classified %v; want none", got.label())
	}
}

// TestIdleAgentNotReportedWithoutQueuedWork is the anti-noise guarantee: a hive
// with nothing to do SHOULD have idle agents, and lighting the facet on those
// would train operators to ignore it.
func TestIdleAgentNotReportedWithoutQueuedWork(t *testing.T) {
	now := time.Now()
	a := AgentSummary{
		Name: "guide", State: "running",
		StartedAt:      settled(now),
		LastActivityAt: activeAt(now, inactiveAgentIdleThreshold+time.Hour),
	}
	if got := classifyInactiveAgent(a, 0, now); got != agentInactiveNone {
		t.Fatalf("idle agent on an empty queue classified %v; want none", got.label())
	}
	// The same agent, with work waiting, IS the signal we want.
	if got := classifyInactiveAgent(a, inactiveAgentMinQueued, now); got != agentInactiveIdleWithWork {
		t.Fatalf("idle agent with queued work classified %v; want idle-with-work", got.label())
	}
}

func TestIdleRuleRespectsThresholdAndOnDemand(t *testing.T) {
	now := time.Now()
	base := AgentSummary{Name: "sec-check", State: "running", StartedAt: settled(now)}

	// Recently active: working, not idle.
	busy := base
	busy.LastActivityAt = activeAt(now, inactiveAgentIdleThreshold-time.Minute)
	if got := classifyInactiveAgent(busy, 50, now); got != agentInactiveNone {
		t.Errorf("recently-active agent classified %v; want none", got.label())
	}

	// On-demand agents are SUPPOSED to sit idle until called.
	onDemand := base
	onDemand.Mode = "on_demand"
	onDemand.LastActivityAt = activeAt(now, 30*24*time.Hour)
	if got := classifyInactiveAgent(onDemand, 50, now); got != agentInactiveNone {
		t.Errorf("on-demand agent classified %v; want none", got.label())
	}
	// But an on-demand agent whose session died is still a fault.
	onDemand.SessionMissing = true
	if got := classifyInactiveAgent(onDemand, 0, now); got != agentInactiveSessionMissing {
		t.Errorf("on-demand zombie classified %v; want session-missing", got.label())
	}
}

// TestUnknownActivityIsNotIdle guards the upgrade path: a spoke too old to
// report lastActivityAt must not make every one of its agents alarm.
func TestUnknownActivityIsNotIdle(t *testing.T) {
	now := time.Now()
	old := AgentSummary{Name: "legacy", State: "running", StartedAt: settled(now)}
	if got := classifyInactiveAgent(old, 999, now); got != agentInactiveNone {
		t.Fatalf("agent with unknown activity classified %v; want none", got.label())
	}
	// A future timestamp is clock skew, not freshness — and not idleness.
	skewed := old
	skewed.LastActivityAt = now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if got := classifyInactiveAgent(skewed, 999, now); got != agentInactiveNone {
		t.Fatalf("future activity classified %v; want none", got.label())
	}
}

func TestNonRunningStatesAreNotThisFacet(t *testing.T) {
	now := time.Now()
	for _, st := range []string{"stopped", "failed", "idle", ""} {
		a := AgentSummary{Name: "x", State: st, StartedAt: settled(now)}
		if got := classifyInactiveAgent(a, 100, now); got != agentInactiveNone {
			t.Errorf("state %q classified %v; want none", st, got.label())
		}
	}
}

// TestStartupGraceSuppressesFreshLaunch stops a restart raising a burst of
// alerts that clear a minute later.
func TestStartupGraceSuppressesFreshLaunch(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Minute).UTC().Format(time.RFC3339)

	// A CLI showing its auth screen seconds after launch is booting, not
	// wedged.
	login := AgentSummary{Name: "ci-maintainer", State: "running", NeedsLogin: true, StartedAt: fresh}
	if got := classifyInactiveAgent(login, 100, now); got != agentInactiveNone {
		t.Fatalf("freshly launched agent on a login prompt classified %v; want none", got.label())
	}

	// The case only the STARTUP grace can suppress: an agent restarted moments
	// ago whose activity clock still carries a stale timestamp from before the
	// relaunch. Without the grace this reads as "idle for hours" and every
	// restart raises a burst of alerts that clear themselves a minute later.
	staleClock := AgentSummary{
		Name: "ci-maintainer", State: "running",
		StartedAt:      fresh,
		LastActivityAt: activeAt(now, inactiveAgentIdleThreshold+3*time.Hour),
	}
	if got := classifyInactiveAgent(staleClock, 100, now); got != agentInactiveNone {
		t.Fatalf("freshly relaunched agent with a stale activity clock classified %v; want none", got.label())
	}
}

func TestEvaluateInactiveAgentsReasonNamesAgents(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		{Name: "scanner", State: "running", SessionMissing: true, StartedAt: settled(now)},
		{Name: "quality", State: "running", NeedsLogin: true, StartedAt: settled(now)},
		// Paused: must not appear anywhere in the output.
		{Name: "architect", State: "paused", Paused: true},
		// Healthy.
		{Name: "guide", State: "running", StartedAt: settled(now), LastActivityAt: activeAt(now, time.Minute)},
	}
	rep := evaluateInactiveAgents(agents, 10, now)
	if rep.Count != 2 {
		t.Fatalf("count = %d, want 2 (reason %q)", rep.Count, rep.Reason)
	}
	for _, want := range []string{"scanner", "session gone", "quality", "not logged in"} {
		if !strings.Contains(rep.Reason, want) {
			t.Errorf("reason %q missing %q", rep.Reason, want)
		}
	}
	if strings.Contains(rep.Reason, "architect") {
		t.Errorf("reason %q names a PAUSED agent", rep.Reason)
	}
	if strings.Contains(rep.Reason, "guide") {
		t.Errorf("reason %q names a healthy agent", rep.Reason)
	}
}

func TestEvaluateInactiveAgentsTruncatesLongLists(t *testing.T) {
	now := time.Now()
	var agents []AgentSummary
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		agents = append(agents, AgentSummary{
			Name: n, State: "running", SessionMissing: true, StartedAt: settled(now),
		})
	}
	rep := evaluateInactiveAgents(agents, 0, now)
	if rep.Count != 5 {
		t.Fatalf("count = %d, want 5", rep.Count)
	}
	if !strings.Contains(rep.Reason, "+2 more") {
		t.Errorf("reason %q should summarise the remainder past %d named", rep.Reason, inactiveAgentsNamedInReason)
	}
}

func TestEvaluateInactiveAgentsEmptyAndUnnamed(t *testing.T) {
	now := time.Now()
	if rep := evaluateInactiveAgents(nil, 100, now); rep.Count != 0 || rep.Reason != "" {
		t.Errorf("nil agents produced %+v; want empty", rep)
	}
	// A nameless agent cannot be actioned, so it is skipped rather than
	// rendering a blank entry in the panel.
	unnamed := []AgentSummary{{Name: "  ", State: "running", SessionMissing: true, StartedAt: settled(now)}}
	if rep := evaluateInactiveAgents(unnamed, 0, now); rep.Count != 0 {
		t.Errorf("unnamed agent counted: %+v", rep)
	}
}

// TestInactiveAgentsAlertRaisedFromPrecomputedFlag checks the wiring into the
// fleet-alert pipeline, including that placeholders are excluded.
func TestInactiveAgentsAlertRaisedFromPrecomputedFlag(t *testing.T) {
	now := time.Now()
	state := newAlertState()
	hives := []alertHive{
		{
			ID: "h1", Name: "org/one", Online: true,
			InactiveAgents: 2, InactiveAgentsReason: "2 agents are running but not working: scanner (session gone), quality (not logged in)",
		},
		{
			// A placeholder pool slot has no agents to be inactive.
			ID: "h2", Name: "available-x", IsPlaceholder: true,
			InactiveAgents: 3, InactiveAgentsReason: "should not be reported",
		},
	}
	summary := evaluateAlerts(state, hives, nil, now)

	var found *Alert
	for i := range summary.Alerts {
		if summary.Alerts[i].Type == AlertTypeAgentsInactive {
			if summary.Alerts[i].HiveID == "h2" {
				t.Fatalf("placeholder hive raised an inactive-agents alert")
			}
			found = &summary.Alerts[i]
		}
	}
	if found == nil {
		t.Fatal("no agents-inactive alert raised for h1")
	}
	if found.Severity != AlertSeverityWarning {
		t.Errorf("severity = %q, want %q", found.Severity, AlertSeverityWarning)
	}
	if !strings.Contains(found.Reason, "scanner") {
		t.Errorf("reason %q lost the spoke-supplied detail", found.Reason)
	}
	if summary.CountsByType[AlertTypeAgentsInactive] != 1 {
		t.Errorf("countsByType = %v, want one agents-inactive", summary.CountsByType)
	}
}

// TestInactiveAgentsAlertIsAckable pairs with the #2348 known-types guard: an
// alert the operator cannot silence is a bug.
func TestInactiveAgentsAlertIsAckable(t *testing.T) {
	if !isKnownAlertType(AlertTypeAgentsInactive) {
		t.Fatal("agents-inactive is not a known alert type, so it cannot be acknowledged")
	}
}
