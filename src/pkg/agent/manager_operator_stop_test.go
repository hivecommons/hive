package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorMayStart_OperatorStop(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", State: StateStopped}
	m.agents["b"] = &AgentProcess{Name: "b", State: StateStopped, OperatorStopped: true}
	m.agents["c"] = &AgentProcess{Name: "c", State: StateIdle}

	if !m.GovernorMayStart("a") {
		t.Fatal("crash/exit-stopped agent must stay eligible for the governor start")
	}
	if m.GovernorMayStart("b") {
		t.Fatal("operator-stopped agent must not be restarted by the governor")
	}
	if m.GovernorMayStart("c") || m.GovernorMayStart("missing") {
		t.Fatal("non-stopped or unknown agents are not start candidates")
	}
}

func TestStopMarksOperatorStopped(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", State: StateStopped}
	if err := m.Stop("a"); err != nil {
		t.Fatal(err)
	}
	if !m.agents["a"].OperatorStopped || m.GovernorMayStart("a") {
		t.Fatal("Stop must mark the agent operator-stopped")
	}
}

func TestStartClearsOperatorStopped(t *testing.T) {
	m := testManager(5)
	m.sandboxConfig = config.AgentSandboxConfig{Enabled: true}
	m.agents["a"] = &AgentProcess{Name: "a", State: StateStopped, OperatorStopped: true}
	if err := m.Start(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if m.agents["a"].OperatorStopped {
		t.Fatal("Start must clear the operator-stop marker")
	}
}
