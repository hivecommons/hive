package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// AuthorizeIssueOpen is the gate for the file-based issue-request relay
// (issue create / comment / claim). It must mirror AuthorizePROpen's
// forge-resistance (the request file's owner must BE the claimed agent)
// while enforcing the weaker CanCreateIssues mode gate. These tests pin
// both halves so a regression cannot silently let an advisory agent write
// issues or let a forged request file impersonate another agent.

func issueOpenManager() *Manager {
	m := testManager(6)
	m.agents["advisor"] = &AgentProcess{Name: "advisor", Config: config.AgentConfig{Mode: "ADVISORY"}}
	m.agents["issuer"] = &AgentProcess{Name: "issuer", Config: config.AgentConfig{Mode: "ISSUES_ONLY"}}
	m.agents["writer"] = &AgentProcess{Name: "writer", Config: config.AgentConfig{Mode: "ISSUES_AND_PRS"}}
	return m
}

func TestAuthorizeIssueOpen_ModeGate(t *testing.T) {
	m := issueOpenManager()

	for _, kind := range []string{"issue", "comment", "claim"} {
		if err := m.AuthorizeIssueOpen("issuer", 0, kind); err != nil {
			t.Errorf("ISSUES_ONLY agent must be allowed kind=%q: %v", kind, err)
		}
		if err := m.AuthorizeIssueOpen("writer", 0, kind); err != nil {
			t.Errorf("ISSUES_AND_PRS agent must be allowed kind=%q: %v", kind, err)
		}
		err := m.AuthorizeIssueOpen("advisor", 0, kind)
		if err == nil {
			t.Errorf("ADVISORY agent must be denied kind=%q", kind)
		} else if !strings.Contains(err.Error(), "may not create issues") {
			t.Errorf("denial should cite the mode gate, got: %v", err)
		}
	}
}

func TestAuthorizeIssueOpen_EmptyAgentName(t *testing.T) {
	m := issueOpenManager()
	for _, name := range []string{"", "   "} {
		if err := m.AuthorizeIssueOpen(name, 0, "issue"); err == nil {
			t.Errorf("empty agent name %q must be denied", name)
		}
	}
}

func TestAuthorizeIssueOpen_UnknownAgent(t *testing.T) {
	m := issueOpenManager()
	err := m.AuthorizeIssueOpen("ghost", 0, "issue")
	if err == nil {
		t.Fatal("unknown agent must be denied")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("denial should cite the unknown agent, got: %v", err)
	}
}

func TestAuthorizeIssueOpen_ForgeResistance(t *testing.T) {
	m := issueOpenManager()
	m.uidMap = NewUIDMap()
	issuerUID := m.uidMap.AllocateUID("issuer")
	writerUID := m.uidMap.AllocateUID("writer")

	// The file owner matching the claimed agent passes.
	if err := m.AuthorizeIssueOpen("issuer", issuerUID, "issue"); err != nil {
		t.Errorf("owner==agent must be allowed: %v", err)
	}

	// A request claiming one agent but owned by another is a forgery.
	err := m.AuthorizeIssueOpen("issuer", writerUID, "issue")
	if err == nil {
		t.Fatal("uid mismatch must be denied")
	}
	if !strings.Contains(err.Error(), "owned by agent") {
		t.Errorf("denial should cite the owning agent, got: %v", err)
	}

	// A UID not registered to any agent is denied outright.
	if err := m.AuthorizeIssueOpen("issuer", 99999, "issue"); err == nil {
		t.Error("unregistered uid must be denied")
	}

	// The forge check happens BEFORE the mode gate: a forged file is
	// rejected as a forgery even when the claimed agent could write issues.
	err = m.AuthorizeIssueOpen("writer", issuerUID, "comment")
	if err == nil || !strings.Contains(err.Error(), "owned by agent") {
		t.Errorf("forge check must precede the mode gate, got: %v", err)
	}
}

func TestAuthorizeIssueOpen_NoUIDMapSkipsForgeCheck(t *testing.T) {
	m := issueOpenManager()
	// With no uidMap (or fileUID<=0) only the mode gate applies.
	if err := m.AuthorizeIssueOpen("issuer", 12345, "issue"); err != nil {
		t.Errorf("nil uidMap must skip forge check: %v", err)
	}
	m.uidMap = NewUIDMap()
	m.uidMap.AllocateUID("issuer")
	if err := m.AuthorizeIssueOpen("issuer", 0, "issue"); err != nil {
		t.Errorf("fileUID=0 must skip forge check: %v", err)
	}
}
