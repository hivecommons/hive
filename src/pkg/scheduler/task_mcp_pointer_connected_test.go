package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The kick's "## Task context MCP" section tells the agent the hive-task
// server is connected, and task_mcp.drop_stuffed_context cuts the work lists
// down to refs because that server replaces them. Both are only true for an
// agent launched as Claude Code, the one CLI the launcher builds an MCP
// configuration for (connectionMCPFlags in crux/agent); these tests pin which
// agents that is.

// taskMCPPointerClaim is how a kick tells the agent it has the server.
const taskMCPPointerClaim = "The `hive-task` MCP server is connected for this launch"

// taskMCPElidedNote is what the pointer adds once the lists are refs only.
const taskMCPElidedNote = "Stuffed work lists in this prompt were elided"

const taskMCPTestURL = "http://hub/api/contribute/mcp"

func taskMCPPointerConfig(agents map[string]config.AgentConfig) *config.Config {
	canaries := false
	cfg := &config.Config{}
	cfg.Project = config.ProjectConfig{Org: "hivecommons", Repos: []string{"hivecommons/hive"}, PrimaryRepo: "hive"}
	cfg.Agents = agents
	cfg.Ioscan = config.IoscanConfig{Canaries: &canaries}
	return cfg
}

func taskMCPPointerWork() *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResultFromItems([]github.Issue{{
			Repo: "hivecommons/hive", Number: 8261, Title: "stuffed issue title", AgeMinutes: 17, Labels: []string{"kind/feature"},
		}}),
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{{Repo: "hivecommons/hive", Number: 8262, Title: "stuffed pr title", Author: "alice"}},
		},
	}
}

// taskMCPKick builds agent's governor kick from cfg, with or without a task
// MCP URL on the scheduler.
func taskMCPKick(t *testing.T, cfg *config.Config, agent string, withURL bool) string {
	t.Helper()
	s := New(cfg, testLogger())
	if withURL {
		s.SetTaskMCPURL(taskMCPTestURL)
	}
	msgs := s.BuildKickMessages(taskMCPPointerWork(), []string{agent})
	if len(msgs) != 1 || msgs[0].Message == "" {
		t.Fatalf("agent %s: got %d kick messages, want one non-empty kick", agent, len(msgs))
	}
	return msgs[0].Message
}

func TestTaskMCPPointerOnlyForBackendsThatLaunchClaude(t *testing.T) {
	// An inference backend or a configured gateway name is launched as the
	// claude CLI, so it has the server too. Nothing else does.
	const gateway = "corp-gateway"
	connected := map[string]bool{"claude": true, gateway: true}
	for _, backend := range config.InferenceBackends {
		connected[backend] = true
	}
	backends := append(config.SupportedBackends(), gateway, "unconfigured-gateway", "")
	told, notTold := 0, 0
	for _, backend := range backends {
		cfg := taskMCPPointerConfig(map[string]config.AgentConfig{"scanner": {Backend: backend}})
		cfg.Governor.Gateways = []config.GatewayConfig{{Name: gateway, Endpoint: "https://gateway.example/v1"}}
		s := New(cfg, testLogger())
		s.SetTaskMCPURL(taskMCPTestURL)
		work := taskMCPPointerWork()
		msg := s.BuildAgentMessage("scanner", work.Issues.Items, work)
		if msg == "" {
			t.Fatalf("backend %q: empty kick", backend)
		}
		got := strings.Contains(msg, taskMCPPointerClaim)
		if got != connected[backend] {
			t.Errorf("backend %q: kick says the hive-task server is connected = %v, want %v", backend, got, connected[backend])
		}
		if connected[backend] {
			told++
		} else {
			notTold++
		}
	}
	// Guard the table itself: claude, the inference backends and the gateway
	// on one side, every other CLI backend plus the two non-backends on the
	// other, so a backend added to either canonical list is covered too.
	wantTold, wantNotTold := len(config.InferenceBackends)+2, len(config.CLIBackends)-1+2
	if told != wantTold || notTold != wantNotTold {
		t.Fatalf("table covered %d connected and %d unconnected backends, want %d and %d", told, notTold, wantTold, wantNotTold)
	}
}

func TestTaskMCPDropStuffedContextNeedsTheConnection(t *testing.T) {
	withFlag := func(backend string) *config.Config {
		return taskMCPPointerConfig(map[string]config.AgentConfig{
			"scanner": {Backend: backend, TaskMCP: &config.AgentTaskMCPConfig{DropStuffedContext: true}},
		})
	}
	for _, backend := range []string{"copilot", "codex", "cline"} {
		kick := taskMCPKick(t, withFlag(backend), "scanner", true)
		for _, forbidden := range []string{taskMCPPointerClaim, taskMCPElidedNote, "context_bundle"} {
			if strings.Contains(kick, forbidden) {
				t.Errorf("backend %s: kick mentions the task MCP (%q) though this agent has no MCP server", backend, forbidden)
			}
		}
		for _, want := range []string{"stuffed issue title", "stuffed pr title"} {
			if !strings.Contains(kick, want) {
				t.Errorf("backend %s: drop_stuffed_context cut %q out of the work lists, leaving bare refs and no MCP to read them from", backend, want)
			}
		}
		// The strongest form: the task MCP URL changes nothing for this agent.
		if old := taskMCPKick(t, withFlag(backend), "scanner", false); kick != old {
			t.Errorf("backend %s: a hive-wide task MCP URL changed the kick of an agent that cannot use it", backend)
		}
	}

	// Control: the flag still elides for an agent that has the server.
	kick := taskMCPKick(t, withFlag("claude"), "scanner", true)
	if !strings.Contains(kick, taskMCPPointerClaim) || !strings.Contains(kick, taskMCPElidedNote) {
		t.Fatalf("claude: kick is missing the task MCP pointer or its elision note:\n%s", kick)
	}
	if strings.Contains(kick, "stuffed issue title") || strings.Contains(kick, "stuffed pr title") {
		t.Fatalf("claude: drop_stuffed_context left stuffed titles in the kick:\n%s", kick)
	}
}

// An agent the hive holds no config for cannot be shown to have the server.
func TestTaskMCPPointerNotForUnconfiguredAgent(t *testing.T) {
	cfg := taskMCPPointerConfig(map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	if kick := taskMCPKick(t, cfg, "helper", true); strings.Contains(kick, taskMCPPointerClaim) {
		t.Error("an agent with no configured backend is told the hive-task server is connected")
	}
	// Control: the configured claude agent beside it still is.
	if kick := taskMCPKick(t, cfg, "scanner", true); !strings.Contains(kick, taskMCPPointerClaim) {
		t.Fatalf("claude agent not pointed at the hive-task server:\n%s", kick)
	}
}

func TestTaskMCPPointerForReplicasAndReviewHeads(t *testing.T) {
	drop := &config.AgentTaskMCPConfig{DropStuffedContext: true}
	codexHead := config.ReviewHead{Backend: "codex", Model: "gpt-5", Alias: "codex"}
	claudeHead := config.ReviewHead{Backend: "claude", Model: "claude-sonnet-5", Alias: "claude"}
	// Which base carries the heads does not matter here: a head is a derived
	// agent marked ReplicaOf its base that runs a backend of its own.
	cfg := taskMCPPointerConfig(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Replicas: 2, TaskMCP: drop, ReviewHeads: []config.ReviewHead{codexHead}},
		"quality": {Backend: "copilot", Replicas: 2, TaskMCP: drop, ReviewHeads: []config.ReviewHead{claudeHead}},
	})
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatalf("ExpandAgentReplicas: %v", err)
	}

	for _, tc := range []struct {
		agent     string
		connected bool
		why       string
	}{
		{"scanner", true, "a claude agent"},
		{"scanner-2", true, "a replica of a claude agent"},
		{"quality", false, "a copilot agent"},
		{"quality-2", false, "a replica of a copilot agent"},
		{config.ReviewHeadAgentName("scanner", codexHead, 1), false, "a codex review head of a claude base"},
		{config.ReviewHeadAgentName("quality", claudeHead, 1), true, "a claude review head of a copilot base"},
	} {
		if _, ok := cfg.Agents[tc.agent]; !ok {
			t.Fatalf("%s: ExpandAgentReplicas did not materialize %s", tc.why, tc.agent)
		}
		kick := taskMCPKick(t, cfg, tc.agent, true)
		if got := strings.Contains(kick, taskMCPPointerClaim); got != tc.connected {
			t.Errorf("%s (%s): kick says the hive-task server is connected = %v, want %v", tc.agent, tc.why, got, tc.connected)
		}
		if got := strings.Contains(kick, taskMCPElidedNote); got != tc.connected {
			t.Errorf("%s (%s): work lists elided = %v, want %v", tc.agent, tc.why, got, tc.connected)
		}
	}
}
