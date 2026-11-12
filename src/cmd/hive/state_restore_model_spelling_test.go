package main

import (
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/cruxtest"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// An agent whose config backend is copilot (its pack default) but whose
// runtime backend override is claude must come back from a restart with the
// Claude CLI's model spelling. The replay used to set the model override
// first, while the effective backend was still the config's copilot, so the
// saved claude-sonnet-5-5 was rewritten to the Copilot spelling
// claude-sonnet-5.5 on every boot, persisted again, and refused by the
// Claude CLI on every turn.
func TestRestoreAgentRuntimeState_ModelSpelledForRestoredBackend(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "copilot", Model: "claude-sonnet-5-5", Enabled: true},
			},
		},
	}

	statePath := filepath.Join(t.TempDir(), "hive-state.json")
	if err := snapshot.SaveState(statePath, &snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {ModelOverride: "claude-sonnet-5-5", BackendOverride: "claude"},
		},
	}, restoreTestLogger()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Two restarts: the value replayed by the first must survive the second.
	for boot := 1; boot <= 2; boot++ {
		saved, err := snapshot.LoadState(statePath, restoreTestLogger())
		if err != nil {
			t.Fatalf("boot %d: LoadState: %v", boot, err)
		}
		m := cruxtest.NewRuntime(cfg.Agents, restoreTestLogger(), crux.ProjectContext{})
		wireGatewayChecker(m, cfg)
		restoreAgentRuntimeState(saved, cfg, m, restoreTestLogger())

		proc, err := m.GetStatus("scanner")
		if err != nil || proc == nil {
			t.Fatalf("boot %d: GetStatus: %v", boot, err)
		}
		if proc.BackendOverride != "claude" {
			t.Fatalf("boot %d: BackendOverride = %q, want claude", boot, proc.BackendOverride)
		}
		if proc.ModelOverride != "claude-sonnet-5-5" {
			t.Fatalf("boot %d: ModelOverride = %q, want claude-sonnet-5-5 (spelled for the restored claude backend, not the config's copilot)", boot, proc.ModelOverride)
		}

		// Persist what is live, as persistState does, for the next boot.
		if err := snapshot.SaveState(statePath, &snapshot.PersistedState{
			Agents: map[string]snapshot.AgentState{
				"scanner": {ModelOverride: proc.ModelOverride, BackendOverride: proc.BackendOverride},
			},
		}, restoreTestLogger()); err != nil {
			t.Fatalf("boot %d: SaveState: %v", boot, err)
		}
	}
}

// A hive-state written before the fix already holds the Copilot spelling for
// a claude agent. The replay re-spells it, so the next persist stores the
// Claude CLI's id and the stale value does not come back.
func TestRestoreAgentRuntimeState_StaleCopilotSpellingSelfHeals(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "claude", Model: "claude-sonnet-5-5", Enabled: true},
			},
		},
	}
	m := cruxtest.NewRuntime(cfg.Agents, restoreTestLogger(), crux.ProjectContext{})
	wireGatewayChecker(m, cfg)

	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {ModelOverride: "claude-sonnet-5.5", BackendOverride: "claude"},
		},
	}, cfg, m, restoreTestLogger())

	proc, _ := m.GetStatus("scanner")
	if proc == nil || proc.ModelOverride != "claude-sonnet-5-5" {
		got := ""
		if proc != nil {
			got = proc.ModelOverride
		}
		t.Errorf("ModelOverride = %q, want claude-sonnet-5-5", got)
	}
}
