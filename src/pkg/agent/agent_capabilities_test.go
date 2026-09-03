package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// AgentCapabilities must return exactly the mode's CanCreateIssues/CanPush/
// CanMerge gates — the same gates AuthorizePROpen/AuthorizeMerge enforce — so a
// hub capability badge can never claim a capability the relay would refuse.
func TestAgentCapabilities_TracksModeGates(t *testing.T) {
	m := testManager(6)
	m.agents["advisor"] = &AgentProcess{Name: "advisor", Config: config.AgentConfig{Mode: "ISSUES_ONLY"}}
	m.agents["writer"] = &AgentProcess{Name: "writer", Config: config.AgentConfig{Mode: "ISSUES_AND_PRS"}}
	m.agents["merger"] = &AgentProcess{Name: "merger", Config: config.AgentConfig{Mode: "ISSUES_PRS_MERGE"}}

	cases := []struct {
		name                         string
		wantIssue, wantPR, wantMerge bool
	}{
		{"advisor", true, false, false},
		{"writer", true, true, false},
		{"merger", true, true, true},
	}
	for _, tc := range cases {
		issue, pr, merge, ok := m.AgentCapabilities(tc.name)
		if !ok {
			t.Fatalf("%s: ok=false, want true", tc.name)
		}
		if issue != tc.wantIssue || pr != tc.wantPR || merge != tc.wantMerge {
			t.Errorf("%s: got (issue=%v pr=%v merge=%v), want (%v %v %v)",
				tc.name, issue, pr, merge, tc.wantIssue, tc.wantPR, tc.wantMerge)
		}

		// The PR/merge results must agree with the actual authorization gates.
		prAllowed := m.AuthorizePROpen(tc.name, 0) == nil
		mergeAllowed := m.AuthorizeMerge(tc.name, 0) == nil
		if pr != prAllowed {
			t.Errorf("%s: CanOpenPR=%v but AuthorizePROpen allowed=%v", tc.name, pr, prAllowed)
		}
		if merge != mergeAllowed {
			t.Errorf("%s: CanMerge=%v but AuthorizeMerge allowed=%v", tc.name, merge, mergeAllowed)
		}
	}
}

func TestAgentCapabilities_UnknownAgent(t *testing.T) {
	m := testManager(6)
	if _, _, _, ok := m.AgentCapabilities("ghost"); ok {
		t.Error("unknown agent must return ok=false")
	}
}

func TestEffectiveBackend_HonorsOverride(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.agents["b"] = &AgentProcess{Name: "b", Config: config.AgentConfig{Backend: "claude"}, BackendOverride: "copilot"}

	if b, ok := m.EffectiveBackend("a"); !ok || b != "claude" {
		t.Errorf("a: got (%q,%v), want (claude,true)", b, ok)
	}
	if b, ok := m.EffectiveBackend("b"); !ok || b != "copilot" {
		t.Errorf("b: override must win; got (%q,%v), want (copilot,true)", b, ok)
	}
	if _, ok := m.EffectiveBackend("ghost"); ok {
		t.Error("unknown agent must return ok=false")
	}
}
