package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// #9445: a CLI wedged mid-turn (the tmux session, the CLI process and the
// pane are all alive — only the model turn is dead) leaves
// state=running/busy=working and deepHealth stall_detection green for hours.
// waitForInputPromptForAgentUnless timing out repeatedly is the only local
// signal, so these tests pin the escalation: kickDeliveryTimeoutEscalateThreshold
// consecutive undeliverable kicks surfaces StructuredStatus=BLOCKED with the
// count and how long ago the wedge was last observed, while a streak below
// the threshold must not.

func wedgedProc(name string, timeouts int, lastAt time.Time) *agent.AgentProcess {
	return &agent.AgentProcess{
		Name:                      name,
		Config:                    config.AgentConfig{Backend: "claude", Enabled: true},
		State:                     agent.StateRunning,
		OutputBuffer:              agent.NewRingBuffer(10),
		KickDeliveryTimeouts:      timeouts,
		LastKickDeliveryTimeoutAt: lastAt,
	}
}

func TestBuildAgents_KickDeliveryWedgeEscalatesToBlocked(t *testing.T) {
	now := time.Now()
	a := buildOneAgent(t, wedgedProc("scanner", kickDeliveryTimeoutEscalateThreshold, now.Add(-5*time.Minute)))

	if a.State != string(agent.StateRunning) || a.Busy != "working" {
		t.Fatalf("state/busy = %q/%q, want running/working", a.State, a.Busy)
	}
	if a.StructuredStatus != "BLOCKED" {
		t.Fatalf("StructuredStatus = %q, want BLOCKED", a.StructuredStatus)
	}
	if !strings.Contains(a.StatusEvidence, "cli-wedged") {
		t.Errorf("StatusEvidence = %q, want it to name cli-wedged", a.StatusEvidence)
	}
	if !strings.Contains(a.StatusEvidence, "3 consecutive kicks undeliverable") {
		t.Errorf("StatusEvidence = %q, want the consecutive-timeout count", a.StatusEvidence)
	}
	if a.LastKickDeliveryTimeoutAt == "" {
		t.Errorf("LastKickDeliveryTimeoutAt not surfaced")
	}
	if a.KickDeliveryTimeouts != kickDeliveryTimeoutEscalateThreshold {
		t.Errorf("KickDeliveryTimeouts = %d, want %d", a.KickDeliveryTimeouts, kickDeliveryTimeoutEscalateThreshold)
	}
}

func TestBuildAgents_KickDeliveryTimeoutsBelowThresholdStaysHealthy(t *testing.T) {
	now := time.Now()
	a := buildOneAgent(t, wedgedProc("scanner", kickDeliveryTimeoutEscalateThreshold-1, now.Add(-time.Minute)))
	if a.StructuredStatus != "" {
		t.Fatalf("StructuredStatus = %q, want empty below threshold", a.StructuredStatus)
	}
}

func TestBuildAgents_KickDeliveryTimeoutsZeroIsHealthy(t *testing.T) {
	// The normal, non-wedged case: an idle agent between kicks never
	// accumulates this counter (reset to 0 the moment a prompt is reached).
	a := buildOneAgent(t, wedgedProc("scanner", 0, time.Time{}))
	if a.StructuredStatus != "" {
		t.Fatalf("StructuredStatus = %q, want empty with no kick-delivery timeouts", a.StructuredStatus)
	}
	if a.LastKickDeliveryTimeoutAt != "" {
		t.Errorf("LastKickDeliveryTimeoutAt = %q, want empty", a.LastKickDeliveryTimeoutAt)
	}
}
