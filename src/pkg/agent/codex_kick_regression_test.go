package agent

import "testing"

// Model an idle Codex exiting on Ctrl+C: no prompt text may reach the shell.
// Exercise both a configured Codex agent and a runtime backend rotation.
func TestCodexKickDoesNotExitCLI(t *testing.T) {
	for _, override := range []bool{false, true} {
		for _, clear := range []bool{false, true} {
			m, agent, _ := kickLogTestManager(t, "")
			if override {
				agent.BackendOverride = "codex"
			} else {
				agent.Config.Backend = "codex"
			}
			agent.Config.ClearOnKick = clear
			alive, delivered := true, false
			termSeams(m).sendKeys = func(_ *AgentProcess, keys ...string) {
				for _, key := range keys {
					if key == "C-c" {
						alive = false
					}
				}
			}
			termSeams(m).captureVisiblePane = func(*AgentProcess) string { return "›" }
			termSeams(m).sendLiteral = func(_ *AgentProcess, text string) {
				if !alive {
					t.Fatalf("kick text reached shell (override=%v, clear=%v): %q", override, clear, text)
				}
				if text == "analyze coverage" {
					delivered = true
				}
			}
			m.deliverKickLocked(agent, "analyze coverage", "send-kick")
			if !delivered {
				t.Fatal("task not delivered")
			}
		}
	}
}
