package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// kickTaskMCPClaim is how a kick tells an agent it has the hive-task server.
const kickTaskMCPClaim = "The `hive-task` MCP server is connected for this launch"

// taskMCPKickBoot runs bootGovernor for a hive with one claude agent and the
// given dashboard token; "" is an open or allowlist-only spoke.
func taskMCPKickBoot(t *testing.T, dashboardToken string) *boot {
	t.Helper()
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets"}
	cfg.Dashboard.PublicURL = "https://hive.example"
	cfg.Dashboard.AuthToken = dashboardToken
	cfg.Agents = map[string]config.AgentConfig{"scanner": {Backend: "claude", Enabled: true}}
	b, _ := newDepsTestBoot(t, cfg)
	b.bootGovernor()
	return b
}

// taskMCPKickSaysConnected builds the scanner's kick and reports whether it
// claims the hive-task server.
func taskMCPKickSaysConnected(t *testing.T, b *boot) bool {
	t.Helper()
	work := &github.ActionableResult{Issues: github.IssueResultFromItems([]github.Issue{{Repo: "acme/widgets", Number: 7, Title: "a task"}})}
	kick := b.sched.BuildAgentMessage("scanner", work.Issues.Items, work)
	if kick == "" {
		t.Fatal("scheduler built an empty kick")
	}
	return strings.Contains(kick, kickTaskMCPClaim)
}

func TestKickPointsAtTaskMCPOnlyWhenALaunchTokenCanBeMinted(t *testing.T) {
	scope := taskmcp.LaunchScope{TaskID: "scanner:acme/widgets#0:1", Repo: "acme/widgets", Agent: "scanner", Generation: 1}

	// No dashboard token: agents are still launched with the endpoint, but no
	// launch token is minted for it and the endpoint refuses every call.
	open := taskMCPKickBoot(t, "")
	if open.taskMCPURLForAgents() == "" || open.taskMCPLaunchTokenForAgents(scope) != "" {
		t.Fatalf("fixture: want a task MCP URL and no launch token, got url=%q token minted=%v",
			open.taskMCPURLForAgents(), open.taskMCPLaunchTokenForAgents(scope) != "")
	}
	if taskMCPKickSaysConnected(t, open) {
		t.Error("a hive that cannot mint a launch token told its agent the hive-task server is connected")
	}

	// Control: with a dashboard token the same agent is pointed at it.
	secured := taskMCPKickBoot(t, "dashboard-secret")
	if secured.taskMCPLaunchTokenForAgents(scope) == "" {
		t.Fatal("fixture: no launch token minted with a dashboard token set")
	}
	if !taskMCPKickSaysConnected(t, secured) {
		t.Error("a claude agent with a mintable launch token was not pointed at the hive-task server")
	}
}

func TestKickTaskMCPPointerFollowsRuntimeBackendOverride(t *testing.T) {
	b := taskMCPKickBoot(t, "dashboard-secret")
	// Before the agent manager exists the configured backend decides.
	if !taskMCPKickSaysConnected(t, b) {
		t.Fatal("claude agent not pointed at the hive-task server before the agent manager exists")
	}

	b.agentMgr = cruxtest.NewRuntime(b.cfg.Agents, b.logger, crux.ProjectContext{},
		cruxtest.WithUIDMapPath(filepath.Join(t.TempDir(), "uid-map.json")))
	if !taskMCPKickSaysConnected(t, b) {
		t.Fatal("claude agent not pointed at the hive-task server once the agent manager exists")
	}

	// Provider rotation moves an agent with SetBackendOverride and leaves its
	// configured backend alone; a copilot launch gets no MCP configuration.
	if err := b.agentMgr.SetBackendOverride("scanner", "copilot"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	if got := b.cfg.Agents["scanner"].Backend; got != "claude" {
		t.Fatalf("fixture: configured backend = %q, want it left at claude", got)
	}
	if taskMCPKickSaysConnected(t, b) {
		t.Error("an agent moved to copilot at runtime is still told the hive-task server is connected")
	}

	if err := b.agentMgr.SetBackendOverride("scanner", "claude"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	if !taskMCPKickSaysConnected(t, b) {
		t.Error("an agent moved back to claude is no longer pointed at the hive-task server")
	}
}
