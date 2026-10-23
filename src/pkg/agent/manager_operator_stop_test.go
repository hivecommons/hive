package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/crux/config"
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
	m.workDir = t.TempDir()
	// SandboxEnabled is an AND of the global switch and the per-agent
	// override; both are needed so Start takes the no-I/O sandbox branch.
	m.sandboxConfig = config.AgentSandboxConfig{Enabled: true}
	enabled := true
	m.agents["a"] = &AgentProcess{
		Name:            "a",
		State:           StateStopped,
		OperatorStopped: true,
		Config:          config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Enabled: &enabled}},
	}
	if err := m.Start(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if m.agents["a"].OperatorStopped {
		t.Fatal("Start must clear the operator-stop marker")
	}
}

func TestRestartClearsOperatorStopped(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", State: StateStopped, OperatorStopped: true}
	_ = m.Restart(t.Context(), "a") // relaunch outcome irrelevant; the marker is cleared first
	if m.agents["a"].OperatorStopped {
		t.Fatal("Restart must clear the operator-stop marker")
	}
	if !m.GovernorMayStart("a") && m.agents["a"].State == StateStopped {
		t.Fatal("restarted agent must be eligible for governor crash recovery")
	}
}
