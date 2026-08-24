package agent

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// acmmLevelIssuesCapable is an ACMM level at which agents may create issues.
// Level 3 defaults agents to ModeIssuesAndPRs, which satisfies
// CanCreateIssues() (>= ModeIssuesOnly).
const acmmLevelIssuesCapable = 3

// ── AuthorizeIssueOpen ──────────────────────────────────────────────────────
//
// These tests mirror the AuthorizePROpen/AuthorizeMerge suites in
// manager_authz_count_test.go: AuthorizeIssueOpen gates the file-based
// issue-request relay, so a regression here silently opens (or closes) the
// issue-write path for every agent.

// TestAuthorizeIssueOpenAllowsIssueCapableAgent is the happy path: an
// issue-capable agent, with a UID map confirming it owns the request file, is
// authorized for each request kind the watcher relays.
func TestAuthorizeIssueOpenAllowsIssueCapableAgent(t *testing.T) {
	for _, kind := range []string{"issue", "comment", "claim"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(acmmLevelIssuesCapable)
			m.agents["quality"] = &AgentProcess{Name: "quality"}
			m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

			if err := m.AuthorizeIssueOpen("quality", testUIDBase, kind); err != nil {
				t.Errorf("AuthorizeIssueOpen(%q) = %v, want nil (quality can create issues at ACMM %d)",
					kind, err, acmmLevelIssuesCapable)
			}
		})
	}
}

// TestAuthorizeIssueOpenRejectsEmptyAgentName covers the first guard: a
// request naming no agent is never authorized.
func TestAuthorizeIssueOpenRejectsEmptyAgentName(t *testing.T) {
	m := testManager(acmmLevelIssuesCapable)

	for _, name := range []string{"", "   "} {
		err := m.AuthorizeIssueOpen(name, testUIDBase, "issue")
		if err == nil {
			t.Fatalf("AuthorizeIssueOpen(%q) = nil, want an error", name)
		}
		if !strings.Contains(err.Error(), "no agent named") {
			t.Errorf("AuthorizeIssueOpen(%q) error = %q, want it to mention the missing agent name", name, err)
		}
	}
}

// TestAuthorizeIssueOpenRejectsForgedAgentName is the core forge check: agent
// A may not file an issue using a request file owned by agent B.
func TestAuthorizeIssueOpenRejectsForgedAgentName(t *testing.T) {
	m := testManager(acmmLevelIssuesCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.agents["scanner"] = &AgentProcess{Name: "scanner"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{
		"quality": testUIDBase,
		"scanner": testUIDBase + 1,
	}}

	// "quality" claims the request, but the file is owned by "scanner"'s UID.
	err := m.AuthorizeIssueOpen("quality", testUIDBase+1, "issue")
	if err == nil {
		t.Fatal("AuthorizeIssueOpen() = nil, want a denial: the file is owned by a different agent")
	}
	if !strings.Contains(err.Error(), "scanner") {
		t.Errorf("error = %q, want it to name the real owning agent", err)
	}
}

// TestAuthorizeIssueOpenRejectsUnknownUID covers a UID present on the file but
// absent from the map — an unregistered process, not an agent.
func TestAuthorizeIssueOpenRejectsUnknownUID(t *testing.T) {
	m := testManager(acmmLevelIssuesCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	err := m.AuthorizeIssueOpen("quality", testUIDBase+99, "issue")
	if err == nil {
		t.Fatal("AuthorizeIssueOpen() = nil, want a denial for an unregistered uid")
	}
	if !strings.Contains(err.Error(), "unknown uid") {
		t.Errorf("error = %q, want it to report an unknown uid", err)
	}
}

// TestAuthorizeIssueOpenSkipsForgeCheckWithoutUIDs documents the deliberate
// fallback: with no UID map, or a non-positive file UID, ownership is
// unverifiable and only the ACMM gate applies. It must NOT hard-fail.
func TestAuthorizeIssueOpenSkipsForgeCheckWithoutUIDs(t *testing.T) {
	t.Run("no uid map", func(t *testing.T) {
		m := testManager(acmmLevelIssuesCapable)
		m.agents["quality"] = &AgentProcess{Name: "quality"}
		// uidMap intentionally nil.
		if err := m.AuthorizeIssueOpen("quality", testUIDBase, "issue"); err != nil {
			t.Errorf("AuthorizeIssueOpen() = %v, want nil (no uid map → ACMM check alone)", err)
		}
	})

	t.Run("non-positive file uid", func(t *testing.T) {
		m := testManager(acmmLevelIssuesCapable)
		m.agents["quality"] = &AgentProcess{Name: "quality"}
		m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}
		if err := m.AuthorizeIssueOpen("quality", 0, "issue"); err != nil {
			t.Errorf("AuthorizeIssueOpen() = %v, want nil (fileUID <= 0 → ACMM check alone)", err)
		}
	})
}

// TestAuthorizeIssueOpenRejectsUnknownAgent covers a well-formed request
// naming an agent this manager does not run.
func TestAuthorizeIssueOpenRejectsUnknownAgent(t *testing.T) {
	m := testManager(acmmLevelIssuesCapable)

	err := m.AuthorizeIssueOpen("ghost", 0, "issue")
	if err == nil {
		t.Fatal("AuthorizeIssueOpen() = nil, want a denial for an unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error = %q, want it to report an unknown agent", err)
	}
}

// TestAuthorizeIssueOpenEnforcesACMMIssueGate is the policy check that matters
// most: an advisory-only agent may not create issues or comments even when the
// forge check passes cleanly.
func TestAuthorizeIssueOpenEnforcesACMMIssueGate(t *testing.T) {
	for _, kind := range []string{"issue", "comment", "claim"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(acmmLevelAdvisoryOnly)
			m.agents["quality"] = &AgentProcess{Name: "quality"}
			m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

			err := m.AuthorizeIssueOpen("quality", testUIDBase, kind)
			if err == nil {
				t.Fatalf("AuthorizeIssueOpen(%q) = nil, want a denial: no agent may create issues at ACMM %d",
					kind, acmmLevelAdvisoryOnly)
			}
			if !strings.Contains(err.Error(), "may not create issues") {
				t.Errorf("error = %q, want it to report the agent may not create issues", err)
			}
		})
	}
}

// TestAuthorizeIssueOpenHonoursExplicitAdvisoryMode checks that an explicit
// per-agent Mode override in config also closes the gate, not just the ACMM
// level default — the override is what an operator sets to demote one agent.
func TestAuthorizeIssueOpenHonoursExplicitAdvisoryMode(t *testing.T) {
	m := testManager(acmmLevelIssuesCapable)
	m.agents["quality"] = &AgentProcess{
		Name:   "quality",
		Config: config.AgentConfig{Mode: ModeAdvisory.String()},
	}

	err := m.AuthorizeIssueOpen("quality", 0, "issue")
	if err == nil {
		t.Fatal("AuthorizeIssueOpen() = nil, want a denial: config Mode pins this agent to advisory")
	}
	if !strings.Contains(err.Error(), "may not create issues") {
		t.Errorf("error = %q, want it to report the agent may not create issues", err)
	}
}
