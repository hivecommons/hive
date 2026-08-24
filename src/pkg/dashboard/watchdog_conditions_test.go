package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/governor"
	"github.com/kubestellar/hive/pkg/watchdog"
)

// TestAgentPayloadCarriesWatchdogConditions verifies the RFC #4665 condition
// set rides the /api/agents payload beside the state echo, and stays absent
// (omitempty) for agents the watchdog has not swept.
func TestAgentPayloadCarriesWatchdogConditions(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
			"quality": {Backend: "codex", Enabled: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{}},
	}
	transition := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			State: agent.StateRunning,
			WatchdogConditions: []watchdog.Condition{
				{Type: watchdog.ConditionReady, Status: watchdog.ConditionFalse, Reason: "shell-prompt", LastTransitionTime: transition},
				{Type: watchdog.ConditionAuthenticated, Status: watchdog.ConditionTrue, Reason: "ProbeOK", LastTransitionTime: transition},
			},
		},
		"quality": {State: agent.StateRunning},
	}

	payload := BuildAgentOnlyStatus(governor.State{Mode: "IDLE"}, statuses, cfg)
	byName := map[string]FrontendAgent{}
	for _, a := range payload.Agents {
		byName[a.Name] = a
	}

	scanner := byName["scanner"]
	if len(scanner.Conditions) != 2 {
		t.Fatalf("scanner conditions = %+v, want 2", scanner.Conditions)
	}
	ready, ok := watchdog.FindCondition(scanner.Conditions, watchdog.ConditionReady)
	if !ok || ready.Status != watchdog.ConditionFalse || ready.Reason != "shell-prompt" {
		t.Fatalf("Ready condition mangled: %+v ok=%v", ready, ok)
	}
	// The observed truth (Ready=False) rides BESIDE the state echo, exactly
	// the RFC's point: state says running, conditions say dead.
	if scanner.State != string(agent.StateRunning) {
		t.Fatalf("state echo = %q", scanner.State)
	}

	raw, err := json.Marshal(byName["quality"])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, present := decoded["conditions"]; present {
		t.Fatal("unswept agent must omit conditions, not fabricate them")
	}
}
