package dashboard

// Regression coverage for the false "needs login" badge on running agents.
//
// Live evidence (hive hosted-llm-d-incubation-llm-d-fast-m-yfzz, pod
// hive-569fbd9f8b-6v99g, v2 @5514dec): /api/health/deep reported scanner and
// quality as {state: running, status: pass} with recent kicks, and a live tmux
// capture of the scanner pane showed Claude Code AUTHENTICATED and mid-work at
// a ready prompt — no login prompt anywhere. The UI still rendered 🔑 Login on
// both. The cause: the shared/legacy credential locations
// (/data/home/.claude/.credentials.json, /data/home/.config/github-copilot/)
// are EMPTY under the per-agent-UID layout, where each agent has its own UID,
// tmux socket and HOME.

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// TestAgentCLIUnauthenticated_InferenceBackendNeverNeedsLogin: the spoke's
// scanner runs METHOD=litellm. An API-key backend has no interactive login, so
// it must never be classified as needing one — regardless of what the
// (shared-path) probe says.
func TestAgentCLIUnauthenticated_InferenceBackendNeverNeedsLogin(t *testing.T) {
	// Probe reports known-unauthenticated for everything — the exact answer an
	// empty shared credential path produces.
	probe := func(string) (bool, bool) { return false, true }

	for _, backend := range config.InferenceBackends {
		for _, state := range []agent.ProcessState{agent.StateRunning, agent.StateStopped} {
			p := &agent.AgentProcess{
				State:  state,
				Config: config.AgentConfig{Backend: backend},
			}
			if agentCLIUnauthenticated(p, probe) {
				t.Fatalf("backend=%s state=%v: inference backend must never need login", backend, state)
			}
		}
	}
}

// TestAgentCLIUnauthenticated_RunningAgentBeatsMissingCredentials: positive
// evidence of health outranks absence-of-file. This is the exact regression —
// a running claude agent whose shared credential path is empty (per-UID home
// holds the real credentials) must not be flagged.
func TestAgentCLIUnauthenticated_RunningAgentBeatsMissingCredentials(t *testing.T) {
	probe := func(string) (bool, bool) { return false, true } // empty shared path

	running := &agent.AgentProcess{
		State:  agent.StateRunning,
		Config: config.AgentConfig{Backend: "claude"},
	}
	if agentCLIUnauthenticated(running, probe) {
		t.Fatal("a running agent must not be flagged needs-login on a missing shared credential file alone")
	}

	// True positive preserved: the same probe verdict on a NON-running agent
	// still reports needs-login.
	stopped := &agent.AgentProcess{
		State:  agent.StateStopped,
		Config: config.AgentConfig{Backend: "claude"},
	}
	if !agentCLIUnauthenticated(stopped, probe) {
		t.Fatal("a stopped agent with no credentials must still report needs-login")
	}
}

// TestAgentCLIUnauthenticated_PaneLoginPromptStillWins: an agent genuinely
// parked at an interactive login prompt must keep showing the badge even while
// its process is running. Rule 2 must not swallow rule 3.
func TestAgentCLIUnauthenticated_PaneLoginPromptStillWins(t *testing.T) {
	probe := func(string) (bool, bool) { return true, true } // credentials look fine

	wedged := &agent.AgentProcess{
		State:      agent.StateRunning,
		NeedsLogin: true,
		Config:     config.AgentConfig{Backend: "claude"},
	}
	if !agentCLIUnauthenticated(wedged, probe) {
		t.Fatal("an agent sitting at a login prompt must always report needs-login")
	}
}
