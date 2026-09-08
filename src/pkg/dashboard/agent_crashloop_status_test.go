package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
)

// #6237: an agent whose CLI died on every launch reported the same
// state=running / busy=working as one doing real work; a live spoke hid a
// seven-agent outage (restart_count 918) behind an all-green dashboard for
// hours. These tests pin the escalation: a restart storm inside the rolling
// 24h window — at the SAME threshold the hub's fleet verdict uses — must
// surface as StructuredStatus=BLOCKED with the count and last crash reason,
// while normal restart churn below the threshold must not.

func crashLoopProc(name string, events []agent.RestartEvent) *agent.AgentProcess {
	return &agent.AgentProcess{
		Name:          name,
		Config:        config.AgentConfig{Backend: "claude", Enabled: true},
		State:         agent.StateRunning,
		OutputBuffer:  agent.NewRingBuffer(10),
		RestartCount:  len(events),
		RestartEvents: events,
	}
}

func buildOneAgent(t *testing.T, proc *agent.AgentProcess) FrontendAgent {
	t.Helper()
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{proc.Name: proc.Config},
	}
	agents := buildAgents(map[string]*agent.AgentProcess{proc.Name: proc}, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 {
		t.Fatalf("buildAgents returned %d agents, want 1", len(agents))
	}
	return agents[0]
}

func TestBuildAgents_CrashLoopEscalatesToBlocked(t *testing.T) {
	now := time.Now()
	var events []agent.RestartEvent
	for i := 0; i < hub.AgentRestartProblemThreshold(); i++ {
		events = append(events, agent.RestartEvent{
			At:     now.Add(-time.Duration(i+1) * time.Minute),
			Reason: "crash",
		})
	}
	a := buildOneAgent(t, crashLoopProc("scanner", events))

	// The raw launch-intent fields keep their historical values — the fix is
	// the escalation channel, not a busy-state rework.
	if a.State != string(agent.StateRunning) || a.Busy != "working" {
		t.Fatalf("state/busy = %q/%q, want running/working", a.State, a.Busy)
	}
	if a.StructuredStatus != "BLOCKED" {
		t.Fatalf("StructuredStatus = %q, want BLOCKED", a.StructuredStatus)
	}
	if !strings.Contains(a.StatusEvidence, "crash-looping") {
		t.Errorf("StatusEvidence = %q, want it to name crash-looping", a.StatusEvidence)
	}
	if !strings.Contains(a.StatusEvidence, "restarts in 24h") {
		t.Errorf("StatusEvidence = %q, want the 24h restart count", a.StatusEvidence)
	}
	if !strings.Contains(a.StatusEvidence, "crash") {
		t.Errorf("StatusEvidence = %q, want the last restart reason", a.StatusEvidence)
	}
	if a.Restarts != len(events) {
		t.Errorf("Restarts = %d, want %d", a.Restarts, len(events))
	}
}

func TestBuildAgents_RestartsBelowThresholdStayHealthy(t *testing.T) {
	now := time.Now()
	var events []agent.RestartEvent
	for i := 0; i < hub.AgentRestartProblemThreshold()-1; i++ {
		events = append(events, agent.RestartEvent{At: now.Add(-time.Duration(i+1) * time.Minute), Reason: "crash"})
	}
	a := buildOneAgent(t, crashLoopProc("scanner", events))
	if a.StructuredStatus != "" {
		t.Fatalf("StructuredStatus = %q, want empty below threshold", a.StructuredStatus)
	}
}

func TestBuildAgents_StaleRestartsOutsideWindowIgnored(t *testing.T) {
	// A long-lived agent accumulates lifetime restarts; only the rolling 24h
	// window may trip the escalation, exactly like the hub's Restarts.Last24h.
	now := time.Now()
	var events []agent.RestartEvent
	for i := 0; i < hub.AgentRestartProblemThreshold()+3; i++ {
		events = append(events, agent.RestartEvent{At: now.Add(-25 * time.Hour), Reason: "crash"})
	}
	proc := crashLoopProc("scanner", events)
	proc.RestartCount = 918
	a := buildOneAgent(t, proc)
	if a.StructuredStatus != "" {
		t.Fatalf("StructuredStatus = %q, want empty for stale restarts", a.StructuredStatus)
	}
	if a.Restarts != 918 {
		t.Errorf("Restarts = %d, want lifetime 918 still reported", a.Restarts)
	}
}

func TestBuildAgents_StartBlockedOutranksCrashLoop(t *testing.T) {
	// A spoke that has STOPPED relaunching carries the more specific reason;
	// it must win over the generic crash-loop evidence.
	now := time.Now()
	var events []agent.RestartEvent
	for i := 0; i < hub.AgentRestartProblemThreshold(); i++ {
		events = append(events, agent.RestartEvent{At: now.Add(-time.Duration(i+1) * time.Minute), Reason: "crash"})
	}
	proc := crashLoopProc("scanner", events)
	proc.StartBlocked = true
	proc.StartFailureReason = "backend binary not found"
	proc.StartFailureCount = 3
	a := buildOneAgent(t, proc)
	if a.StructuredStatus != "BLOCKED" {
		t.Fatalf("StructuredStatus = %q, want BLOCKED", a.StructuredStatus)
	}
	if !strings.Contains(a.StatusEvidence, "backend binary not found") {
		t.Errorf("StatusEvidence = %q, want the start-failure reason to win", a.StatusEvidence)
	}
}

func TestRestartsLast24h_ReasonFallsBackToLastRestartReason(t *testing.T) {
	proc := crashLoopProc("scanner", []agent.RestartEvent{
		{At: time.Now().Add(-time.Minute)},
	})
	proc.LastRestartReason = "session lock EACCES"
	n, reason := restartsLast24h(proc, time.Now())
	if n != 1 || reason != "session lock EACCES" {
		t.Fatalf("restartsLast24h = (%d, %q), want (1, session lock EACCES)", n, reason)
	}
}
