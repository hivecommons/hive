package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// AuthorizePushBranch is the gate for the file-based push-branch relay
// (hivecommons/hive#9771). It must mirror AuthorizePROpen exactly — the same
// file-UID forge-resistance and the same CanPush ACMM gate — so the relay
// grants no privilege over the direct `git push` it stands in for. These tests
// pin both halves.

func pushBranchManager() *Manager {
	m := testManager(6)
	m.agents["advisor"] = &AgentProcess{Name: "advisor", Config: config.AgentConfig{Mode: "ADVISORY"}}
	m.agents["issuer"] = &AgentProcess{Name: "issuer", Config: config.AgentConfig{Mode: "ISSUES_ONLY"}}
	m.agents["writer"] = &AgentProcess{Name: "writer", Config: config.AgentConfig{Mode: "ISSUES_AND_PRS"}}
	return m
}

func TestAuthorizePushBranch_ModeGate(t *testing.T) {
	m := pushBranchManager()

	if err := m.AuthorizePushBranch("writer", 0); err != nil {
		t.Errorf("push-capable agent must be allowed: %v", err)
	}
	for _, name := range []string{"advisor", "issuer"} {
		err := m.AuthorizePushBranch(name, 0)
		if err == nil {
			t.Errorf("%s must be denied: not push-capable", name)
		} else if !strings.Contains(err.Error(), "not push-capable") {
			t.Errorf("denial should cite the push gate, got: %v", err)
		}
	}
}

// The gate must agree with AuthorizePROpen for every agent: both stand in for
// the same CanPush write tier, so an agent allowed one but not the other would
// be a policy split with no rationale.
func TestAuthorizePushBranch_MirrorsAuthorizePROpen(t *testing.T) {
	m := pushBranchManager()
	for name := range m.agents {
		pr := m.AuthorizePROpen(name, 0) == nil
		push := m.AuthorizePushBranch(name, 0) == nil
		if pr != push {
			t.Errorf("%s: AuthorizePROpen allowed=%v but AuthorizePushBranch allowed=%v", name, pr, push)
		}
	}
}

func TestAuthorizePushBranch_RejectsEmptyAgentName(t *testing.T) {
	m := pushBranchManager()
	for _, name := range []string{"", "   "} {
		if err := m.AuthorizePushBranch(name, 5000); err == nil {
			t.Errorf("AuthorizePushBranch(%q) = nil, want an error", name)
		}
	}
}

// The core forge check: agent A may not push using a request file owned by
// agent B, and a file owned by an unregistered UID is refused outright.
func TestAuthorizePushBranch_ForgeResistance(t *testing.T) {
	m := pushBranchManager()
	m.uidMap = &UIDMap{BaseUID: 5000, Agents: map[string]int{
		"writer": 5000,
		"issuer": 5001,
	}}

	if err := m.AuthorizePushBranch("writer", 5000); err != nil {
		t.Errorf("owner's own request denied: %v", err)
	}
	err := m.AuthorizePushBranch("writer", 5001)
	if err == nil {
		t.Fatal("a request file owned by another agent's UID was accepted")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Errorf("error = %q, want it to name the real owning agent", err)
	}
	if err := m.AuthorizePushBranch("writer", 4999); err == nil ||
		!strings.Contains(err.Error(), "unknown uid") {
		t.Errorf("unregistered file owner accepted or misreported: %v", err)
	}
}

func TestAuthorizePushBranch_UnknownAgent(t *testing.T) {
	m := pushBranchManager()
	if err := m.AuthorizePushBranch("ghost", 0); err == nil ||
		!strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("unknown agent accepted or misreported: %v", err)
	}
}
