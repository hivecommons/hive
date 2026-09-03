package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// acmmLevelPushCapable is an ACMM level at which the "quality" agent defaults
// to ModeIssuesAndPRs (push-capable). See DefaultAgentMode.
const acmmLevelPushCapable = 3

// acmmLevelAdvisoryOnly is an ACMM level at which every agent defaults to
// ModeAdvisory, so no agent may open a PR.
const acmmLevelAdvisoryOnly = 1

// testUIDBase is an arbitrary non-root base UID for the synthetic UID maps in
// these tests; the exact value is irrelevant as long as it is > 0.
const testUIDBase = 5000

// ── CountAgentsWithModel ────────────────────────────────────────────────────

// TestCountAgentsWithModel pins the documented contract: an agent counts when
// EITHER a backend or a model is set, from the config or from any of the three
// override fields. Only a wholly empty backend AND model reads as unassigned.
func TestCountAgentsWithModel(t *testing.T) {
	tests := []struct {
		name  string
		agent *AgentProcess
		want  bool
	}{
		{
			name:  "backend from config counts",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}},
			want:  true,
		},
		{
			name:  "model from config counts",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Model: "opus"}},
			want:  true,
		},
		{
			name:  "backend override counts when config is empty",
			agent: &AgentProcess{Name: "a", BackendOverride: "copilot"},
			want:  true,
		},
		{
			name:  "model override counts when config is empty",
			agent: &AgentProcess{Name: "a", ModelOverride: "sonnet"},
			want:  true,
		},
		{
			name:  "pinned model counts when config is empty",
			agent: &AgentProcess{Name: "a", PinnedModel: "haiku"},
			want:  true,
		},
		{
			name:  `"auto" is a deliberate routing selection, not an absence`,
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Model: "auto"}},
			want:  true,
		},
		{
			name:  `"default" is a deliberate routing selection, not an absence`,
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "default"}},
			want:  true,
		},
		{
			name:  "wholly empty does not count",
			agent: &AgentProcess{Name: "a"},
			want:  false,
		},
		{
			name:  "whitespace-only backend and model do not count",
			agent: &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "  ", Model: "\t"}},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(acmmLevelPushCapable)
			m.agents["a"] = tc.agent

			got := m.CountAgentsWithModel()
			want := 0
			if tc.want {
				want = 1
			}
			if got != want {
				t.Errorf("CountAgentsWithModel() = %d, want %d", got, want)
			}
		})
	}
}

// TestCountAgentsWithModelSkipsNil guards the nil-entry skip: a nil map value
// must not be counted and must not panic.
func TestCountAgentsWithModelSkipsNil(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["assigned"] = &AgentProcess{Name: "assigned", Config: config.AgentConfig{Backend: "claude"}}
	m.agents["nil-entry"] = nil
	m.agents["unassigned"] = &AgentProcess{Name: "unassigned"}

	if got := m.CountAgentsWithModel(); got != 1 {
		t.Errorf("CountAgentsWithModel() = %d, want 1 (only the assigned agent)", got)
	}
}

// TestCountAgentsWithModelOverrideBeatsEmptyConfig checks that an override on a
// config-less agent is what makes it count — i.e. the override fields are
// really consulted rather than the config alone.
func TestCountAgentsWithModelOverrideBeatsEmptyConfig(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	a := &AgentProcess{Name: "a"}
	m.agents["a"] = a

	if got := m.CountAgentsWithModel(); got != 0 {
		t.Fatalf("precondition: unassigned agent counted %d, want 0", got)
	}
	a.ModelOverride = "opus"
	if got := m.CountAgentsWithModel(); got != 1 {
		t.Errorf("after setting ModelOverride: got %d, want 1", got)
	}
}

// ── AuthorizePROpen ─────────────────────────────────────────────────────────

// TestAuthorizePROpenAllowsPushCapableAgent is the happy path: a push-capable
// agent, with a UID map that confirms it owns the request file, is authorized.
func TestAuthorizePROpenAllowsPushCapableAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	if err := m.AuthorizePROpen("quality", testUIDBase); err != nil {
		t.Errorf("AuthorizePROpen() = %v, want nil (quality is push-capable at ACMM %d)", err, acmmLevelPushCapable)
	}
}

// TestAuthorizePROpenRejectsEmptyAgentName covers the first guard: a request
// naming no agent is never authorized.
func TestAuthorizePROpenRejectsEmptyAgentName(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	for _, name := range []string{"", "   "} {
		err := m.AuthorizePROpen(name, testUIDBase)
		if err == nil {
			t.Fatalf("AuthorizePROpen(%q) = nil, want an error", name)
		}
		if !strings.Contains(err.Error(), "no agent named") {
			t.Errorf("AuthorizePROpen(%q) error = %q, want it to mention the missing agent name", name, err)
		}
	}
}

// TestAuthorizePROpenRejectsForgedAgentName is the core forge check: agent A
// may not open a PR using a request file owned by agent B.
func TestAuthorizePROpenRejectsForgedAgentName(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.agents["scanner"] = &AgentProcess{Name: "scanner"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{
		"quality": testUIDBase,
		"scanner": testUIDBase + 1,
	}}

	// "quality" claims the request, but the file is owned by "scanner"'s UID.
	err := m.AuthorizePROpen("quality", testUIDBase+1)
	if err == nil {
		t.Fatal("AuthorizePROpen() = nil, want a denial: the file is owned by a different agent")
	}
	if !strings.Contains(err.Error(), "scanner") {
		t.Errorf("error = %q, want it to name the real owning agent", err)
	}
}

// TestAuthorizePROpenRejectsUnknownUID covers a UID present on the file but
// absent from the map — an unregistered process, not an agent.
func TestAuthorizePROpenRejectsUnknownUID(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	err := m.AuthorizePROpen("quality", testUIDBase+99)
	if err == nil {
		t.Fatal("AuthorizePROpen() = nil, want a denial for an unregistered uid")
	}
	if !strings.Contains(err.Error(), "unknown uid") {
		t.Errorf("error = %q, want it to report an unknown uid", err)
	}
}

// TestAuthorizePROpenSkipsForgeCheckWithoutUIDs documents the deliberate
// fallback: with no UID map, or a non-positive file UID, ownership is
// unverifiable and only the ACMM gate applies. It must NOT hard-fail.
func TestAuthorizePROpenSkipsForgeCheckWithoutUIDs(t *testing.T) {
	t.Run("no uid map", func(t *testing.T) {
		m := testManager(acmmLevelPushCapable)
		m.agents["quality"] = &AgentProcess{Name: "quality"}
		// uidMap intentionally nil.
		if err := m.AuthorizePROpen("quality", testUIDBase); err != nil {
			t.Errorf("AuthorizePROpen() = %v, want nil (no uid map → ACMM check alone)", err)
		}
	})

	t.Run("non-positive file uid", func(t *testing.T) {
		m := testManager(acmmLevelPushCapable)
		m.agents["quality"] = &AgentProcess{Name: "quality"}
		m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}
		if err := m.AuthorizePROpen("quality", 0); err != nil {
			t.Errorf("AuthorizePROpen() = %v, want nil (fileUID <= 0 → ACMM check alone)", err)
		}
	})
}

// TestAuthorizePROpenRejectsUnknownAgent covers a well-formed request naming an
// agent this manager does not run.
func TestAuthorizePROpenRejectsUnknownAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	err := m.AuthorizePROpen("ghost", 0)
	if err == nil {
		t.Fatal("AuthorizePROpen() = nil, want a denial for an unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error = %q, want it to report an unknown agent", err)
	}
}

// TestAuthorizePROpenEnforcesACMMWriteGate is the policy check that matters
// most: an advisory-only agent may not open a PR even when the forge check
// passes cleanly. This mirrors the `gh pr create` CanPush() gate.
func TestAuthorizePROpenEnforcesACMMWriteGate(t *testing.T) {
	m := testManager(acmmLevelAdvisoryOnly)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	err := m.AuthorizePROpen("quality", testUIDBase)
	if err == nil {
		t.Fatalf("AuthorizePROpen() = nil, want a denial: no agent is push-capable at ACMM %d", acmmLevelAdvisoryOnly)
	}
	if !strings.Contains(err.Error(), "not push-capable") {
		t.Errorf("error = %q, want it to report the agent is not push-capable", err)
	}
}

// TestAuthorizePROpenHonoursExplicitAdvisoryMode checks that an explicit
// per-agent Mode override in config also closes the gate, not just the ACMM
// level default — the override is what an operator sets to demote one agent.
func TestAuthorizePROpenHonoursExplicitAdvisoryMode(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{
		Name:   "quality",
		Config: config.AgentConfig{Mode: ModeAdvisory.String()},
	}

	err := m.AuthorizePROpen("quality", 0)
	if err == nil {
		t.Fatal("AuthorizePROpen() = nil, want a denial: config Mode pins this agent to advisory")
	}
	if !strings.Contains(err.Error(), "not push-capable") {
		t.Errorf("error = %q, want it to report the agent is not push-capable", err)
	}
}

// ── AuthorizeMerge ──────────────────────────────────────────────────────────

// TestAuthorizeMergeAllowsMergeCapableAgent: an agent whose mode grants merge
// (ISSUES_PRS_MERGE), owning its request file, is authorized to merge.
func TestAuthorizeMergeAllowsMergeCapableAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Mode: "ISSUES_PRS_MERGE"}}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"scanner": testUIDBase}}

	if err := m.AuthorizeMerge("scanner", testUIDBase); err != nil {
		t.Errorf("AuthorizeMerge() = %v, want nil (scanner mode ISSUES_PRS_MERGE is merge-capable)", err)
	}
}

// TestAuthorizeMergeRejectsNonMergeAgent: an agent that can open PRs but is only
// ISSUES_AND_PRS (not merge) must be denied — merging is a strictly higher gate.
func TestAuthorizeMergeRejectsNonMergeAgent(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality", Config: config.AgentConfig{Mode: "ISSUES_AND_PRS"}}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	err := m.AuthorizeMerge("quality", testUIDBase)
	if err == nil {
		t.Fatal("AuthorizeMerge() = nil, want denial: ISSUES_AND_PRS is push-capable but not merge-capable")
	}
	if !strings.Contains(err.Error(), "not merge-capable") {
		t.Errorf("error = %q, want it to report the agent is not merge-capable", err)
	}
}

// TestAuthorizeMergeRejectsForgedAgentName: the same forge anchor as PR-open —
// an agent may not merge using a request file owned by a different agent.
func TestAuthorizeMergeRejectsForgedAgentName(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Mode: "ISSUES_PRS_MERGE"}}
	m.agents["quality"] = &AgentProcess{Name: "quality", Config: config.AgentConfig{Mode: "ISSUES_PRS_MERGE"}}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{
		"scanner": testUIDBase,
		"quality": testUIDBase + 1,
	}}

	err := m.AuthorizeMerge("scanner", testUIDBase+1) // file owned by quality's UID
	if err == nil {
		t.Fatal("AuthorizeMerge() = nil, want a denial: file owned by a different agent")
	}
	if !strings.Contains(err.Error(), "quality") {
		t.Errorf("error = %q, want it to name the real owning agent", err)
	}
}

// TestAuthorizeMergeRejectsEmptyAgentName: a request naming no agent is denied.
func TestAuthorizeMergeRejectsEmptyAgentName(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	if err := m.AuthorizeMerge("  ", testUIDBase); err == nil {
		t.Fatal("AuthorizeMerge(\"  \") = nil, want an error")
	}
}
