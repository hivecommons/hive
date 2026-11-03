package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// An agent with agents[].model: "" must never be shown (or saved) a launch
// command with a bare `--model`: on the claude path that flag swallows
// --dangerously-skip-permissions and the CLI blocks on permission dialogs.
func TestDefaultLaunchCmdForBackend_EmptyModelOmitsModelFlag(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	for _, backend := range []string{"copilot", "claude", "litellm", "codex", "pi"} {
		for _, model := range []string{"", "  "} {
			cmd := s.defaultLaunchCmdForBackend(backend, model)
			if strings.Contains(cmd, "--model") {
				t.Errorf("%s with model %q rendered --model: %q", backend, model, cmd)
			}
			if _, dropped := config.DropDanglingModelFlag(cmd); dropped {
				t.Errorf("%s with model %q rendered a dangling --model: %q", backend, model, cmd)
			}
		}
	}
	if got, want := s.defaultLaunchCmdForBackend("claude", ""), "claude --dangerously-skip-permissions"; got != want {
		t.Errorf("claude empty model = %q, want %q", got, want)
	}
	if got, want := s.defaultLaunchCmdForBackend("claude", "claude-opus-4-8"), "claude --model claude-opus-4-8 --dangerously-skip-permissions"; got != want {
		t.Errorf("claude configured model = %q, want %q", got, want)
	}
	if got, want := s.defaultLaunchCmdForBackend("copilot", "auto"), "/usr/bin/copilot --allow-all --model auto"; got != want {
		t.Errorf("copilot = %q, want %q", got, want)
	}
}
