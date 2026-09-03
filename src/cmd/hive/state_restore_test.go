package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// These tests pin the #3961 restore half: runtime overrides persisted to
// /data/hive-state.json must actually be live again after a pod restart.
// The replay itself lived inline in main() and its failures were silent —
// SetBackendOverride's error was discarded while "backend override restored"
// was logged unconditionally, so a gateway-named backend override that failed
// validation (the gateway predicate was wired AFTER the replay in the old
// boot order) vanished with a success message. The model override beside it
// restored fine, producing the launch-dead hybrid from the issue
// (`pi --model gpt-5.6-luna`).

func restoreTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wireGatewayChecker mirrors main()'s (post-fix) wiring: the predicate is
// attached right after the manager is built, BEFORE any state replay.
func wireGatewayChecker(m *agent.Manager, cfg *config.Config) {
	m.SetGatewayBackendChecker(func(backend string) bool {
		return cfg.Governor.ResolveGateway(backend) != nil && backend != ""
	})
}

// TestRestoreAgentRuntimeState_SurvivesRestart is the full restart-survival
// round trip: persist agent state (as persistState does), re-load it from
// disk (as boot does), replay it onto a fresh manager, and assert every
// override is live — including a backend override naming a configured model
// GATEWAY, the case the old boot order dropped.
func TestRestoreAgentRuntimeState_SurvivesRestart(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Repos: []string{"r"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Gateways: []config.GatewayConfig{
				{Name: "corp-litellm", Kind: "litellm", Endpoint: "http://litellm.example:4000"},
			},
		},
	}

	statePath := filepath.Join(t.TempDir(), "hive-state.json")
	if err := snapshot.SaveState(statePath, &snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {
				Paused:          true,
				PausedTrigger:   "dashboard-api",
				PausedReason:    "manual pause",
				ModelOverride:   "deepseek-v4-flash",
				BackendOverride: "corp-litellm",
				RestartCount:    3,
			},
		},
	}, restoreTestLogger()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// "Pod restart": fresh manager, state re-loaded from disk.
	saved, err := snapshot.LoadState(statePath, restoreTestLogger())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	restoreAgentRuntimeState(saved, cfg, m, restoreTestLogger())

	proc, err := m.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.BackendOverride != "corp-litellm" {
		t.Errorf("BackendOverride = %q after restart, want %q (gateway-named backend override dropped on replay — #3961)", proc.BackendOverride, "corp-litellm")
	}
	if proc.ModelOverride != "deepseek-v4-flash" {
		t.Errorf("ModelOverride = %q after restart, want %q", proc.ModelOverride, "deepseek-v4-flash")
	}
	if !proc.Paused {
		t.Error("operator pause did not survive the restart")
	}
	if proc.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want dashboard-api (an operator pause must stay attributed, or Start's auto-unpause may drop it)", proc.PausedTrigger)
	}
	if proc.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3", proc.RestartCount)
	}
}

// TestRestoreAgentRuntimeState_CLIBackendOverride is the positive control:
// a plain CLI backend override never depended on the gateway predicate and
// must keep restoring exactly as before.
func TestRestoreAgentRuntimeState_CLIBackendOverride(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {BackendOverride: "copilot"},
		},
	}, cfg, m, restoreTestLogger())

	proc, _ := m.GetStatus("scanner")
	if proc.BackendOverride != "copilot" {
		t.Errorf("BackendOverride = %q, want copilot", proc.BackendOverride)
	}
}

// TestRestoreAgentRuntimeState_DeletedGatewayNotRestored: the one legitimate
// replay rejection — the saved override names a gateway that no longer exists
// in the config. The override must be dropped (the agent falls back to its
// config backend) WITHOUT taking the model override or the rest of the
// agent's state down with it.
func TestRestoreAgentRuntimeState_DeletedGatewayNotRestored(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
		// No gateways configured: "corp-litellm" is unresolvable. (Deliberately
		// NOT a CLI backend name — those pass validation without any gateway.)
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {BackendOverride: "corp-litellm", ModelOverride: "deepseek-v4-flash", RestartCount: 1},
		},
	}, cfg, m, restoreTestLogger())

	proc, _ := m.GetStatus("scanner")
	if proc.BackendOverride != "" {
		t.Errorf("BackendOverride = %q, want empty (gateway no longer configured)", proc.BackendOverride)
	}
	if proc.ModelOverride != "deepseek-v4-flash" {
		t.Errorf("ModelOverride = %q, want deepseek-v4-flash (must restore independently of the rejected backend)", proc.ModelOverride)
	}
	if proc.RestartCount != 1 {
		t.Errorf("RestartCount = %d, want 1", proc.RestartCount)
	}
}
