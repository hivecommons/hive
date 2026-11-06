package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/taskmcp"
)

// A hive always hands the manager its task MCP endpoint (cmd/hive
// taskMCPURLForAgents), and every registration path then adds a hive-task
// connection to the stored config. These tests register Cline agents the way
// a hive does, instead of building AgentProcess by hand.
const (
	clineTaskMCPURL       = "http://localhost:3002" + taskmcp.EndpointPath
	clinePolicyRefusal    = "tools/connections configuration"
	clineNoSandboxRefusal = "Cline has no OS sandbox"
)

func clineTaskMCPProject() ProjectContext {
	return ProjectContext{Org: "acme", PrimaryRepoName: "widgets", TaskMCPURL: clineTaskMCPURL}
}

func clinePlainConfig() config.AgentConfig {
	return config.AgentConfig{Backend: "cline", Model: "openrouter/anthropic/test"}
}

// clineTmuxLaunchError runs the tmux launch path for a registered agent and
// reports where it stopped. A test manager has no per-UID worker, so a launch
// that gets past the policy check stops at the "no OS sandbox" refusal, before
// anything is executed.
func clineTmuxLaunchError(t *testing.T, m *Manager, name string) error {
	t.Helper()
	t.Setenv("HIVE_CLINE_DANGEROUSLY_RUN_UNCONFINED", "")
	m.mu.Lock()
	defer m.mu.Unlock()
	agent := m.agents[name]
	if agent == nil {
		t.Fatalf("fixture: agent %s is not registered", name)
	}
	return m.launchInTmux(context.Background(), agent)
}

func TestClineAgentPassesLaunchPolicyWithHiveInjectedTaskMCPConnection(t *testing.T) {
	stubUIDMapPath(t)
	for _, tc := range []struct {
		name     string
		register func(t *testing.T) *Manager
	}{
		{"NewManager", func(*testing.T) *Manager {
			return NewManager(map[string]config.AgentConfig{"worker": clinePlainConfig()}, discardLogger(), clineTaskMCPProject())
		}},
		{"AddAgent", func(*testing.T) *Manager {
			m := NewManager(nil, discardLogger(), clineTaskMCPProject())
			m.AddAgent("worker", clinePlainConfig())
			return m
		}},
		{"ReconcileAgents adds the agent", func(*testing.T) *Manager {
			m := NewManager(nil, discardLogger(), clineTaskMCPProject())
			m.ReconcileAgents(map[string]config.AgentConfig{"worker": clinePlainConfig()})
			return m
		}},
		// A dashboard save stores the operator's config as written; the next
		// config reload reconciles and adds the connection back.
		{"ReconcileAgents refreshes a known agent", func(t *testing.T) *Manager {
			m := NewManager(map[string]config.AgentConfig{"worker": clinePlainConfig()}, discardLogger(), clineTaskMCPProject())
			if err := m.UpdateConfig("worker", clinePlainConfig()); err != nil {
				t.Fatal(err)
			}
			m.ReconcileAgents(map[string]config.AgentConfig{"worker": clinePlainConfig()})
			return m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.register(t)
			m.mu.RLock()
			conns := m.agents["worker"].Config.Connections
			m.mu.RUnlock()
			if len(conns) != 1 || conns[0].Type != "mcp" || conns[0].Name != "hive-task" {
				t.Fatalf("fixture: stored connections = %#v, want only the hive-injected hive-task connection", conns)
			}
			err := clineTmuxLaunchError(t, m, "worker")
			if err == nil || strings.Contains(err.Error(), clinePolicyRefusal) || !strings.Contains(err.Error(), clineNoSandboxRefusal) {
				t.Fatalf("launch of a Cline agent with no operator tools or connections stopped at: %v\nwant it past the tools/connections policy check, at the %q refusal", err, clineNoSandboxRefusal)
			}
		})
	}
}

func TestClineAgentWithOperatorToolsOrConnectionsStillRefused(t *testing.T) {
	stubUIDMapPath(t)
	for _, tc := range []struct {
		name      string
		conn      config.ConnectionConfig // the operator's connection, unless tools is set
		tools     bool
		noTaskMCP bool // the hive has no task MCP endpoint, so it injects nothing
	}{
		{name: "mcp connection", conn: config.ConnectionConfig{Name: "tasks", Type: "mcp", URI: "https://mcp.example.test/tasks"}},
		{name: "api connection", conn: config.ConnectionConfig{Name: "tracker", Type: "api", URI: "https://api.example.test"}},
		{name: "hive-task on another host", conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: "http://elsewhere.example.test:3002" + taskmcp.EndpointPath}},
		{name: "hive-task on another path", conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: "http://localhost:3002/api/other"}},
		{name: "hive-task of another type", conn: config.ConnectionConfig{Name: "hive-task", Type: "api", URI: clineTaskMCPURL}},
		{name: "hive-task with its own auth", conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: clineTaskMCPURL,
			Auth: &config.ConnectionAuth{Type: "env", EnvVar: "TASK_MCP_TOKEN"}}},
		{name: "hive-task with its own env name", conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: clineTaskMCPURL,
			EnvName: "TASK_MCP_URL"}},
		{name: "hive-task with its own options", conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: clineTaskMCPURL,
			Options: map[string]string{"transport": "sse"}}},
		{name: "hive-task on a hive without a task MCP endpoint", noTaskMCP: true,
			conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp", URI: clineTaskMCPURL}},
		{name: "hive-task without a URI on a hive without a task MCP endpoint", noTaskMCP: true,
			conn: config.ConnectionConfig{Name: "hive-task", Type: "mcp"}},
		{name: "tools", tools: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := clinePlainConfig()
			if tc.tools {
				cfg.Tools = &config.ToolsConfig{Preset: "read-only"}
			} else {
				cfg.Connections = []config.ConnectionConfig{tc.conn}
			}
			project := clineTaskMCPProject()
			if tc.noTaskMCP {
				project.TaskMCPURL = ""
			}
			m := NewManager(map[string]config.AgentConfig{"worker": cfg}, discardLogger(), project)
			if err := clineTmuxLaunchError(t, m, "worker"); err == nil || !strings.Contains(err.Error(), clinePolicyRefusal) {
				t.Fatalf("launch error = %v, want the tools/connections policy refusal", err)
			}
		})
	}
}

func TestClineSandboxKickPreflightWithHiveInjectedTaskMCPConnection(t *testing.T) {
	stubUIDMapPath(t)
	for _, tc := range []struct {
		name    string
		mutate  func(*config.AgentConfig)
		refused bool
	}{
		{name: "no operator tools or connections"},
		{name: "operator connection", refused: true, mutate: func(c *config.AgentConfig) {
			c.Connections = []config.ConnectionConfig{{Name: "tasks", Type: "mcp", URI: "https://mcp.example.test/tasks"}}
		}},
		{name: "tools", refused: true, mutate: func(c *config.AgentConfig) { c.Tools = &config.ToolsConfig{Preset: "read-only"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yes := true
			cfg := clinePlainConfig()
			cfg.Sandbox = &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			m := NewManager(map[string]config.AgentConfig{"worker": cfg}, discardLogger(), clineTaskMCPProject())
			m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "cline-image", WorkspaceDir: t.TempDir(),
				Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data", EnvFromSecrets: []string{"dedicated-cline-provider"}}})
			if err := m.Start(context.Background(), "worker"); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			_, err := m.validateSandboxKickLocked(m.agents["worker"])
			m.mu.Unlock()
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), clinePolicyRefusal) {
					t.Fatalf("sandbox kick preflight = %v, want the tools/connections policy refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sandbox kick of a Cline agent with no operator tools or connections refused: %v", err)
			}
		})
	}
}
