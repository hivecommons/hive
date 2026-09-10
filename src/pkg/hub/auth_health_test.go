package hub

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

func agentAt(name, status string, since time.Time) AgentSummary {
	return AgentSummary{
		Name:              name,
		Enabled:           true,
		BackendAuthStatus: status,
		BackendAuthSince:  since.UTC().Format(time.RFC3339),
	}
}

func TestEvaluateAuthHealth_NoEnabledAgentsIsOK(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		{Name: "scanner", Enabled: false, BackendAuthStatus: agent.BackendAuthUnlicensed},
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthOK {
		t.Fatalf("Status = %q, want %q (no enabled agents)", rep.Status, AuthHealthOK)
	}
}

func TestEvaluateAuthHealth_AllOKIsOK(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		{Name: "scanner", Enabled: true},
		{Name: "outreach", Enabled: true, BackendAuthStatus: agent.BackendAuthOK},
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthOK {
		t.Fatalf("Status = %q, want %q", rep.Status, AuthHealthOK)
	}
}

func TestEvaluateAuthHealth_SomeFailingIsDegraded(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		agentAt("scanner", agent.BackendAuthUnlicensed, now.Add(-30*time.Minute)),
		{Name: "outreach", Enabled: true},
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDegraded {
		t.Fatalf("Status = %q, want %q", rep.Status, AuthHealthDegraded)
	}
	if rep.Reason == "" {
		t.Fatal("expected a non-empty reason for a degraded hive")
	}
}

func TestEvaluateAuthHealth_AllFailingBelowThresholdIsDegraded(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		agentAt("scanner", agent.BackendAuthUnlicensed, now.Add(-5*time.Minute)),
		agentAt("outreach", agent.BackendAuthUnlicensed, now.Add(-3*time.Minute)),
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDegraded {
		t.Fatalf("Status = %q, want %q (below the down threshold)", rep.Status, AuthHealthDegraded)
	}
}

// TestEvaluateAuthHealth_AllFailingPastThresholdIsDown is the #6500 shape:
// every enabled agent has been unlicensed for well over the default 15
// minutes. Since must be the LATEST per-agent BackendAuthSince — the moment
// the fleet-wide condition became true, not the first agent to fail.
func TestEvaluateAuthHealth_AllFailingPastThresholdIsDown(t *testing.T) {
	now := time.Now()
	firstFailed := now.Add(-40 * time.Minute)
	lastFailed := now.Add(-20 * time.Minute)
	agents := []AgentSummary{
		agentAt("scanner", agent.BackendAuthUnlicensed, firstFailed),
		agentAt("outreach", agent.BackendAuthUnlicensed, lastFailed),
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDown {
		t.Fatalf("Status = %q, want %q", rep.Status, AuthHealthDown)
	}
	wantSince, _ := time.Parse(time.RFC3339, lastFailed.UTC().Format(time.RFC3339))
	if !rep.Since.Equal(wantSince) {
		t.Fatalf("Since = %v, want the LATEST failure time %v", rep.Since, wantSince)
	}
}

// TestEvaluateAuthHealth_RightAtThreshold verifies the boundary: down uses
// the same >= convention as evaluateInactiveAgents' idle rule beside it — a
// hive AT the threshold is already down, matching "N minutes is long enough"
// rather than "must exceed".
func TestEvaluateAuthHealth_RightAtThreshold(t *testing.T) {
	now := time.Now()
	since := now.Add(-AuthHealthDownThreshold())
	agents := []AgentSummary{agentAt("scanner", agent.BackendAuthUnlicensed, since)}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDown {
		t.Fatalf("Status at exactly the threshold = %q, want %q", rep.Status, AuthHealthDown)
	}

	belowThreshold := now.Add(-AuthHealthDownThreshold() + time.Minute)
	agents = []AgentSummary{agentAt("scanner", agent.BackendAuthUnlicensed, belowThreshold)}
	rep = evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDegraded {
		t.Fatalf("Status one minute short of threshold = %q, want %q", rep.Status, AuthHealthDegraded)
	}
}

func TestEvaluateAuthHealth_MissingSinceNeverReadsAsDown(t *testing.T) {
	now := time.Now()
	// A legacy spoke reports status without a parseable timestamp: must never
	// be read as "just started failing" (which would silently gate down
	// forever) NOR immediately down (unknown duration is not evidence).
	agents := []AgentSummary{
		{Name: "scanner", Enabled: true, BackendAuthStatus: agent.BackendAuthUnlicensed},
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDegraded {
		t.Fatalf("Status with no parseable Since = %q, want %q", rep.Status, AuthHealthDegraded)
	}
}

func TestEvaluateAuthHealth_QuotaAndUnreachableClassesCount(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	agents := []AgentSummary{
		agentAt("scanner", agent.BackendAuthQuota, past),
		agentAt("outreach", agent.BackendAuthUnreachable, past),
	}
	rep := evaluateAuthHealth(agents, now)
	if rep.Status != AuthHealthDown {
		t.Fatalf("Status = %q, want %q", rep.Status, AuthHealthDown)
	}
}

func TestAuthHealthDownThreshold_EnvOverride(t *testing.T) {
	t.Setenv(EnvAuthHealthDownThreshold, "5m")
	if got := AuthHealthDownThreshold(); got != 5*time.Minute {
		t.Fatalf("AuthHealthDownThreshold() = %v, want 5m", got)
	}
}

func TestAuthHealthDownThreshold_InvalidFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvAuthHealthDownThreshold, "not-a-duration")
	if got := AuthHealthDownThreshold(); got != DefaultAuthHealthDownThreshold {
		t.Fatalf("AuthHealthDownThreshold() = %v, want default %v", got, DefaultAuthHealthDownThreshold)
	}
}

func TestAuthHealthReason_NamesUpToLimitThenSummarizes(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{
		agentAt("a", agent.BackendAuthUnlicensed, now),
		agentAt("b", agent.BackendAuthUnlicensed, now),
		agentAt("c", agent.BackendAuthUnlicensed, now),
		agentAt("d", agent.BackendAuthUnlicensed, now),
	}
	reason := authHealthReason(agents, 4)
	if got := "4 of 4 enabled agents are failing backend auth: a (unlicensed), b (unlicensed), c (unlicensed) (+1 more)"; reason != got {
		t.Fatalf("reason = %q, want %q", reason, got)
	}
}
