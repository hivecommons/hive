package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
)

func forgeCloneManager() *Manager {
	m := pushBranchManager()
	m.uidMap = &UIDMap{BaseUID: 5000, Agents: map[string]int{
		"writer":  5000,
		"issuer":  5001,
		"advisor": 5002,
	}}
	return m
}

// A clone is a read: every registered agent may ask, at its own token tier,
// including an advisory one that may not push.
func TestAuthorizeForgeCloneTierPerAgent(t *testing.T) {
	m := forgeCloneManager()
	for name, uid := range map[string]int{"writer": 5000, "advisor": 5002} {
		tier, err := m.AuthorizeForgeClone(name, uid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := TokenTierForRole(m.agentMode(m.agents[name]), m.agents[name].Config.Role)
		if tier != want {
			t.Fatalf("%s: tier %q, want %q", name, tier, want)
		}
	}
	if tier, _ := m.AuthorizeForgeClone("advisor", 5002); tier != agentmode.TokenTierAdvisor {
		t.Fatalf("advisor tier = %q, want %q", tier, agentmode.TokenTierAdvisor)
	}
}

// Identity is checked against the file owner, never taken from the request,
// and there is no shared-UID fallback.
func TestAuthorizeForgeCloneFailsClosedOnIdentity(t *testing.T) {
	m := forgeCloneManager()
	for name, c := range map[string]struct {
		agent string
		uid   int
		want  string
	}{
		"empty name":       {"  ", 5000, "no agent"},
		"no owner uid":     {"writer", 0, "per-agent UIDs"},
		"unreadable owner": {"writer", -1, "per-agent UIDs"},
		"unknown uid":      {"writer", 4999, "unknown uid"},
		"other agent":      {"writer", 5001, `owned by agent "issuer"`},
	} {
		if tier, err := m.AuthorizeForgeClone(c.agent, c.uid); err == nil || tier != "" || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: tier %q err %v, want a denial naming %q", name, tier, err, c.want)
		}
	}
	m.uidMap.Agents["ghost"] = 5009
	if _, err := m.AuthorizeForgeClone("ghost", 5009); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("mapped but unknown agent: %v", err)
	}
	m.uidMap = nil
	if _, err := m.AuthorizeForgeClone("writer", 5000); err == nil || !strings.Contains(err.Error(), "per-agent UIDs") {
		t.Errorf("no UID map: %v", err)
	}
}
