package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// These tests cover the restoreAgentRuntimeState branches the #3961 tests
// left unexercised: the not-in-config skip, the default pause provenance,
// CLI/model pins, restart telemetry, turn-loss and kick-history seeding, and
// the dashboard-editable config field merge. Each is state that silently
// fails to survive a pod restart if its replay branch regresses — exactly
// the failure mode restoreAgentRuntimeState was factored out to catch.

// TestRestoreAgentRuntimeState_SkipsAgentNotInConfig: saved state for an
// agent that was since removed from the config must be skipped without
// creating a phantom agent on the manager.
func TestRestoreAgentRuntimeState_SkipsAgentNotInConfig(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"ghost": {Paused: true, ModelOverride: "deepseek-v4-flash"},
		},
	}, cfg, m, restoreTestLogger())

	if _, err := m.GetStatus("ghost"); err == nil {
		t.Error("GetStatus(ghost) succeeded — saved state for an agent not in config must not materialize a phantom agent")
	}
	if proc, _ := m.GetStatus("scanner"); proc == nil || proc.Paused {
		t.Error("scanner state disturbed by a skipped ghost-agent replay")
	}
}

// TestRestoreAgentRuntimeState_DefaultPauseProvenance: a paused agent whose
// snapshot predates pause provenance (empty reason/trigger) must restore
// paused with the documented fallbacks, and its PausedAt must be re-seeded so
// pause age survives the restart rather than resetting to boot time.
func TestRestoreAgentRuntimeState_DefaultPauseProvenance(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	pausedAt := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {Paused: true, PausedAt: &pausedAt},
		},
	}, cfg, m, restoreTestLogger())

	proc, err := m.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !proc.Paused {
		t.Fatal("pause did not survive the restart")
	}
	if proc.PausedTrigger != "state-restore" {
		t.Errorf("PausedTrigger = %q, want the state-restore fallback", proc.PausedTrigger)
	}
	if proc.PausedReason != "persisted pause state" {
		t.Errorf("PausedReason = %q, want the persisted-pause fallback", proc.PausedReason)
	}
	if !proc.PausedAt.Equal(pausedAt) {
		t.Errorf("PausedAt = %v, want the persisted %v (pause age must not reset on restart)", proc.PausedAt, pausedAt)
	}
}

// TestRestoreAgentRuntimeState_PinsAndTelemetry: CLI/model pins, restart
// telemetry (events + last reason), last kick, kick history, and turn-loss
// accounting must all be live on the manager after replay.
func TestRestoreAgentRuntimeState_PinsAndTelemetry(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	lastKick := time.Now().Add(-time.Hour).Truncate(time.Second)
	eventAt := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	sinceOutput := 4.5
	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {
				PinnedCLI:   "1.2.3",
				PinnedModel: "sonnet-pinned",
				RestartEvents: []snapshot.AgentRestartEvent{
					{At: eventAt, Reason: "stale"},
				},
				LastRestartReason: "stale",
				LastKick:          &lastKick,
				KickHistory: []snapshot.AgentKickEntry{
					{Timestamp: lastKick, Agent: "scanner", Snippet: "scan the repo"},
				},
				TurnLoss: &snapshot.AgentTurnLoss{
					Interruptions: 2,
					Producing:     1,
					UpperBoundS:   90,
					Bytes:         2048,
					Recent: []snapshot.AgentTurnInterruption{
						{At: eventAt, Reason: "upgrade", SinceKickS: 60, SinceOutputS: &sinceOutput, Producing: true, Bytes: 1024},
					},
				},
			},
		},
	}, cfg, m, restoreTestLogger())

	proc, err := m.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.PinnedCLI != "1.2.3" {
		t.Errorf("PinnedCLI = %q, want 1.2.3", proc.PinnedCLI)
	}
	if proc.PinnedModel != "sonnet-pinned" {
		t.Errorf("PinnedModel = %q, want sonnet-pinned", proc.PinnedModel)
	}
	if proc.LastRestartReason != "stale" {
		t.Errorf("LastRestartReason = %q, want stale", proc.LastRestartReason)
	}
	if len(proc.RestartEvents) != 1 || proc.RestartEvents[0].Reason != "stale" {
		t.Errorf("RestartEvents = %+v, want the single persisted stale event", proc.RestartEvents)
	}
	if proc.LastKick == nil || !proc.LastKick.Equal(lastKick) {
		t.Errorf("LastKick = %v, want %v", proc.LastKick, lastKick)
	}
	if len(proc.KickHistory) != 1 || proc.KickHistory[0].Snippet != "scan the repo" {
		t.Errorf("KickHistory = %+v, want the single persisted kick", proc.KickHistory)
	}
	if proc.TurnLoss.Interruptions != 2 || proc.TurnLoss.Producing != 1 {
		t.Errorf("TurnLoss counts = %d/%d, want 2/1 (turn-loss accounting must survive the restart it measures)",
			proc.TurnLoss.Interruptions, proc.TurnLoss.Producing)
	}
	if proc.TurnLoss.UpperBound != 90*time.Second {
		t.Errorf("TurnLoss.UpperBound = %v, want 90s", proc.TurnLoss.UpperBound)
	}
	if proc.TurnLoss.Bytes != 2048 {
		t.Errorf("TurnLoss.Bytes = %d, want 2048", proc.TurnLoss.Bytes)
	}
	if len(proc.TurnLoss.Recent) != 1 {
		t.Fatalf("TurnLoss.Recent has %d records, want 1", len(proc.TurnLoss.Recent))
	}
	rec := proc.TurnLoss.Recent[0]
	if rec.SinceKick != 60*time.Second || !rec.Producing || rec.Bytes != 1024 {
		t.Errorf("TurnLoss.Recent[0] = %+v, want the persisted interruption", rec)
	}
	if rec.SinceOutput == nil || *rec.SinceOutput != time.Duration(4.5*float64(time.Second)) {
		t.Errorf("TurnLoss.Recent[0].SinceOutput = %v, want 4.5s (nil means UNKNOWN and must round-trip)", rec.SinceOutput)
	}
}

// TestRestoreAgentRuntimeState_ConfigFieldMerge: the dashboard-editable
// config fields ride the state file. Persisted DisplayName/Description fill
// in only when the config leaves them empty — a config-file value must win —
// while Enabled/ClearOnKick/StaleTimeout/RestartStrategy always replay.
func TestRestoreAgentRuntimeState_ConfigFieldMerge(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {
				Backend:     "claude",
				Enabled:     true,
				DisplayName: "Config Name", // must win over the persisted one
			},
		},
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	wireGatewayChecker(m, cfg)

	enabled := false
	clearOnKick := true
	staleTimeout := 900
	restoreAgentRuntimeState(&snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {
				DisplayName:     "Persisted Name",
				Description:     "persisted description",
				Enabled:         &enabled,
				ClearOnKick:     &clearOnKick,
				StaleTimeout:    &staleTimeout,
				RestartStrategy: "never",
			},
		},
	}, cfg, m, restoreTestLogger())

	got := cfg.Agents["scanner"]
	if got.DisplayName != "Config Name" {
		t.Errorf("DisplayName = %q, want the config value to win over the persisted one", got.DisplayName)
	}
	if got.Description != "persisted description" {
		t.Errorf("Description = %q, want the persisted value to fill the empty config field", got.Description)
	}
	if got.Enabled {
		t.Error("Enabled = true, want the persisted dashboard disable to replay")
	}
	if !got.ClearOnKick {
		t.Error("ClearOnKick = false, want the persisted value to replay")
	}
	if got.StaleTimeout != staleTimeout {
		t.Errorf("StaleTimeout = %d, want %d", got.StaleTimeout, staleTimeout)
	}
	if got.RestartStrategy != "never" {
		t.Errorf("RestartStrategy = %q, want never", got.RestartStrategy)
	}
}
