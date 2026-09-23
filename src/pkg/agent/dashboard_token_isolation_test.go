package agent

import (
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #8348: a hub-launched agent must never be handed the dashboard bearer, in
// its environment or in the MCP server flags of its launch command. On v5
// the manager has no task-MCP URL at all, so the invariant is that nothing
// the launch path derives from the process environment or the project
// context carries HIVE_DASHBOARD_TOKEN's value.

// dashboardTokenSentinel is a value that cannot occur by accident in any
// env pair or launch flag, so a substring match is conclusive.
const dashboardTokenSentinel = "hive-dashboard-token-sentinel-8348"

// agentMCPConnectionURI is the operator-declared MCP server the positive
// control expects to see, verbatim, in the launch flags.
const agentMCPConnectionURI = "https://mcp.example.test/tasks"

func newDashboardTokenIsolationManager(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	t.Setenv("HIVE_DASHBOARD_TOKEN", dashboardTokenSentinel)
	p := ProjectContext{
		Org:             "acme",
		Repos:           []string{"widgets"},
		PrimaryRepoName: "acme/widgets",
	}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			Backend: "claude",
			Model:   "claude-sonnet-5",
			Connections: []config.ConnectionConfig{
				{Name: "tasks", Type: "mcp", URI: agentMCPConnectionURI},
			},
		},
	}, discardLogger(), p)
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if agent == nil {
		t.Fatal("fixture agent not registered")
	}
	return m, agent
}

func TestAgentEnvNeverCarriesDashboardToken(t *testing.T) {
	m, agent := newDashboardTokenIsolationManager(t)

	pairs := m.agentEnvPairs(agent)
	if len(pairs) == 0 {
		t.Fatal("positive control: agentEnvPairs returned nothing")
	}
	sawAgentName := false
	for _, pair := range pairs {
		if pair.Key == "HIVE_DASHBOARD_TOKEN" {
			t.Errorf("HIVE_DASHBOARD_TOKEN exported to the agent environment")
		}
		if strings.Contains(pair.Value, dashboardTokenSentinel) {
			t.Errorf("env %s carries the dashboard token: %q", pair.Key, pair.Value)
		}
		if pair.Key == "HIVE_AGENT" && pair.Value == agent.Name {
			sawAgentName = true
		}
	}
	if !sawAgentName {
		t.Fatal("positive control: HIVE_AGENT not exported, env builder did not run")
	}

	prefix := m.buildEnvPrefix(agent)
	if strings.Contains(prefix, dashboardTokenSentinel) {
		t.Errorf("env prefix carries the dashboard token: %q", prefix)
	}
	if !strings.Contains(prefix, "HIVE_AGENT=") {
		t.Fatalf("positive control: env prefix missing HIVE_AGENT: %q", prefix)
	}
}

func TestAgentMCPFlagsNeverCarryDashboardToken(t *testing.T) {
	_, agent := newDashboardTokenIsolationManager(t)

	flags := connectionMCPFlags(agent.Config.Connections, "claude")
	if strings.Contains(flags, dashboardTokenSentinel) {
		t.Fatalf("MCP launch flags carry the dashboard token: %q", flags)
	}

	// Positive control: the flag is well formed and names exactly the
	// operator-declared server, with no token query parameter appended.
	const flagPrefix = " --mcp-server '"
	if !strings.HasPrefix(flags, flagPrefix) || !strings.HasSuffix(flags, "'") {
		t.Fatalf("MCP launch flags malformed: %q", flags)
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(flags, flagPrefix), "'")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("MCP server URL does not parse: %q: %v", raw, err)
	}
	if u.String() != agentMCPConnectionURI {
		t.Fatalf("MCP server URL = %q, want %q", u.String(), agentMCPConnectionURI)
	}
	if u.Query().Has("token") {
		t.Fatalf("MCP server URL carries a token query parameter: %q", raw)
	}
}
