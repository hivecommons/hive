package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// boolPtr is a local helper: AgentConfig.Converse is a *bool so that "unset"
// stays distinguishable from "explicitly false".
func boolPtr(b bool) *bool { return &b }

// advisoryConverseAgent builds the shape AuthorizeReviewRequest exists for:
// mode ADVISORY (not push-capable) plus an explicit `converse: true`.
func advisoryConverseAgent(name string, converse *bool) *AgentProcess {
	return &AgentProcess{Name: name, Config: config.AgentConfig{Converse: converse}}
}

// TestAuthorizeReviewRequestAllowsAdvisoryConverseAgent is the whole point of
// the change: an ADVISORY agent with `converse: true` may submit a review
// through the sanctioned relay. Before this, the relay demanded CanPush() and
// an advisory reviewer could reach a verdict with no way to say it.
func TestAuthorizeReviewRequestAllowsAdvisoryConverseAgent(t *testing.T) {
	m := testManager(acmmLevelAdvisoryOnly)
	m.agents["reviewer"] = advisoryConverseAgent("reviewer", boolPtr(true))
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"reviewer": testUIDBase}}

	if mode := m.agentMode(m.agents["reviewer"]); mode.CanPush() {
		t.Fatalf("precondition: reviewer is push-capable (mode %s) at ACMM %d; "+
			"this test must exercise the NON-push path", mode, acmmLevelAdvisoryOnly)
	}
	if err := m.AuthorizeReviewRequest("reviewer", testUIDBase); err != nil {
		t.Errorf("AuthorizeReviewRequest() = %v, want nil (converse grants conversation writes)", err)
	}
}

// TestAuthorizeReviewRequestRejectsAdvisoryWithoutConverse pins the other side:
// converse is opt-in, so an advisory agent that was never granted it stays
// silent. Without this, the change would read as "advisory agents may now
// review", which is not what it does.
func TestAuthorizeReviewRequestRejectsAdvisoryWithoutConverse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		converse *bool
	}{
		{"unset", nil},
		{"explicitly false", boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(acmmLevelAdvisoryOnly)
			m.agents["reviewer"] = advisoryConverseAgent("reviewer", tc.converse)
			m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"reviewer": testUIDBase}}

			err := m.AuthorizeReviewRequest("reviewer", testUIDBase)
			if err == nil {
				t.Fatalf("AuthorizeReviewRequest() = nil, want an error (converse is opt-in)")
			}
			if !strings.Contains(err.Error(), "converse") {
				t.Errorf("error = %q, want it to name the missing `converse` capability so an "+
					"operator knows which knob to turn", err)
			}
		})
	}
}

// TestAuthorizeReviewRequestAllowsPushCapableWithoutConverse is the
// no-regression case: mode alone still authorizes, exactly as AuthorizePROpen
// did before. Capability is an ADDITIONAL grant path, not a replacement.
func TestAuthorizeReviewRequestAllowsPushCapableWithoutConverse(t *testing.T) {
	m := testManager(acmmLevelPushCapable)
	m.agents["quality"] = &AgentProcess{Name: "quality"}
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{"quality": testUIDBase}}

	if mode := m.agentMode(m.agents["quality"]); !mode.CanPush() {
		t.Fatalf("precondition: quality is not push-capable (mode %s) at ACMM %d", mode, acmmLevelPushCapable)
	}
	if err := m.AuthorizeReviewRequest("quality", testUIDBase); err != nil {
		t.Errorf("AuthorizeReviewRequest() = %v, want nil (push-capable mode still authorizes)", err)
	}
}

// TestAuthorizeReviewRequestRejectsForgedAgentName keeps the forge-resistance
// the other authorizers enforce: converse must not become a way to review as
// somebody else. Agent A writes the file; agent B claims it.
func TestAuthorizeReviewRequestRejectsForgedAgentName(t *testing.T) {
	m := testManager(acmmLevelAdvisoryOnly)
	m.agents["reviewer"] = advisoryConverseAgent("reviewer", boolPtr(true))
	m.agents["scanner"] = advisoryConverseAgent("scanner", boolPtr(true))
	m.uidMap = &UIDMap{BaseUID: testUIDBase, Agents: map[string]int{
		"reviewer": testUIDBase,
		"scanner":  testUIDBase + 1,
	}}

	// reviewer claims a request file actually owned by scanner.
	err := m.AuthorizeReviewRequest("reviewer", testUIDBase+1)
	if err == nil {
		t.Fatal("AuthorizeReviewRequest() = nil, want an error (file is owned by scanner)")
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("error = %q, want it to explain the ownership mismatch", err)
	}
}

// TestAuthorizeReviewRequestRejectsEmptyAndUnknownAgents covers the two
// fail-closed guards.
func TestAuthorizeReviewRequestRejectsEmptyAndUnknownAgents(t *testing.T) {
	m := testManager(acmmLevelPushCapable)

	for _, name := range []string{"", "   "} {
		err := m.AuthorizeReviewRequest(name, testUIDBase)
		if err == nil {
			t.Fatalf("AuthorizeReviewRequest(%q) = nil, want an error", name)
		}
		if !strings.Contains(err.Error(), "no agent named") {
			t.Errorf("AuthorizeReviewRequest(%q) error = %q, want it to mention the missing name", name, err)
		}
	}

	err := m.AuthorizeReviewRequest("ghost", 0)
	if err == nil {
		t.Fatal("AuthorizeReviewRequest(\"ghost\") = nil, want an error for an unregistered agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error = %q, want it to say the agent is unknown", err)
	}
}
