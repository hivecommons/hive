package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Provider rotation (and a restored backend switch) change the backend an
// agent launches on without touching its config, so the task MCP pointer
// follows the agent runtime's answer when the host attaches one.
func TestTaskMCPPointerFollowsRuntimeBackend(t *testing.T) {
	drop := &config.AgentTaskMCPConfig{DropStuffedContext: true}
	newConfig := func() *config.Config {
		cfg := taskMCPPointerConfig(map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Replicas: 2, TaskMCP: drop},
			"quality": {Backend: "copilot", TaskMCP: drop},
		})
		if err := cfg.ExpandAgentReplicas(); err != nil {
			t.Fatalf("ExpandAgentReplicas: %v", err)
		}
		return cfg
	}

	for _, tc := range []struct {
		name string
		// runtime maps an agent to the backend the runtime launches it on; an
		// agent missing from it is unknown to the runtime.
		runtime   map[string]string
		agent     string
		connected bool
	}{
		{"claude agent moved to copilot", map[string]string{"scanner": "copilot"}, "scanner", false},
		{"copilot agent moved to claude", map[string]string{"quality": "claude"}, "quality", true},
		{"copilot agent moved to an inference backend", map[string]string{"quality": "litellm"}, "quality", true},
		{"runtime does not know the agent, configured claude", nil, "scanner", true},
		{"runtime does not know the agent, configured copilot", nil, "quality", false},
		{"replica moved on its own", map[string]string{"scanner-2": "codex"}, "scanner-2", false},
		{"a moved replica leaves its base alone", map[string]string{"scanner-2": "codex"}, "scanner", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(newConfig(), testLogger())
			s.SetTaskMCPURL(taskMCPTestURL)
			s.SetAgentBackendLookup(func(agent string) (string, bool) {
				backend, ok := tc.runtime[agent]
				return backend, ok
			})
			msgs := s.BuildKickMessages(taskMCPPointerWork(), []string{tc.agent})
			if len(msgs) != 1 || msgs[0].Message == "" {
				t.Fatalf("got %d kick messages, want one non-empty kick", len(msgs))
			}
			kick := msgs[0].Message
			if got := strings.Contains(kick, taskMCPPointerClaim); got != tc.connected {
				t.Errorf("kick says the hive-task server is connected = %v, want %v", got, tc.connected)
			}
			if got := strings.Contains(kick, taskMCPElidedNote); got != tc.connected {
				t.Errorf("work lists elided = %v, want %v", got, tc.connected)
			}
		})
	}
}

// A hive with no usable task MCP endpoint points no agent at it, so it has
// nothing to ask the runtime (a sidecar runtime answers over HTTP).
func TestTaskMCPPointerDoesNotAskRuntimeWithoutEndpoint(t *testing.T) {
	cfg := taskMCPPointerConfig(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", TaskMCP: &config.AgentTaskMCPConfig{DropStuffedContext: true}},
	})
	s := New(cfg, testLogger())
	asked := 0
	s.SetAgentBackendLookup(func(string) (string, bool) {
		asked++
		return "claude", true
	})
	msgs := s.BuildKickMessages(taskMCPPointerWork(), []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("got %d kick messages, want 1", len(msgs))
	}
	if strings.Contains(msgs[0].Message, taskMCPPointerClaim) {
		t.Fatal("kick points at the hive-task server though the hive has no task MCP URL")
	}
	if asked != 0 {
		t.Fatalf("runtime backend lookup called %d times with no task MCP URL, want 0", asked)
	}
}
