package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// A model-rejection pause must come back from a hive restart with its
// trigger and reason, because a later model change lifts only a pause whose
// trigger says the CLI rejected the model. And a pause the operator lifted
// must not come back at all.
//
// On the old code the second restart below brought the agent back paused with
// no trigger, reason, actor or time: the operator's model edit had written the
// agent's overlay file while it was paused, the resume never rewrote that
// file, and the overlay's stale paused bit won at load while hive-state.json
// (which recorded the resume) had nothing to restore. The next persist then
// wrote {"paused": true} with no provenance at all.

const provenanceTestRejectReason = "model claude-sonnet-5.5 rejected by the CLI 3 times in a row; set a valid model for this agent (the change resumes it)"

func TestModelRejectedPauseProvenanceSurvivesHiveRestart(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldRuntimeConfig, oldDashboardOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile, config.DashboardOverlayFile = oldRuntimeConfig, oldDashboardOverlay
	})

	cfgPath := filepath.Join(dir, "hive.yaml")
	agentsDir := filepath.Join(dir, "agent-configs")
	statePath := filepath.Join(dir, "hive-state.json")
	paths := persistStateTestPaths(dir)
	cfg := &config.Config{
		SourcePath: cfgPath,
		Project:    config.ProjectConfig{Org: "testorg", Repos: []string{"repo"}},
		GitHub:     config.GitHubConfig{AppID: 1234567},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"worker": {Backend: "claude", Model: "claude-sonnet-5.5", Enabled: true},
			},
		},
		Data: config.DataConfig{AgentsDir: agentsDir},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// main.go's pause persistence callback.
	wirePausePersist := func(m cruxtest.Runtime) {
		m.SetPersistPauseCallback(func(name string, paused bool) {
			if _, err := cfg.SetAgentPausedAndSave(name, paused); err != nil {
				t.Errorf("persist pause: %v", err)
			}
		})
	}
	persist := func(m crux.Runtime) {
		gov := governor.New(cfg.Governor, cfg.Agents, logger)
		persistStateWithPaths(m, gov, cfg, statePath, logger, nil, nil, paths)
	}
	// restart reloads hive.yaml (with its agent overlays) and hive-state.json
	// from disk into a fresh runtime, as a hive boot does.
	restart := func() *crux.Process {
		t.Helper()
		reloaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		saved, err := snapshot.LoadState(statePath, logger)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		m := cruxtest.NewRuntime(reloaded.Agents, logger, crux.ProjectContext{})
		if saved != nil {
			restoreAgentRuntimeState(saved, reloaded, m, logger)
		}
		st, err := m.GetStatus("worker")
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		return st
	}

	// The CLI rejected the model three times; the runtime parked the agent,
	// and the governor tick persisted the state.
	live := cruxtest.NewRuntime(cfg.Agents, logger, crux.ProjectContext{})
	wirePausePersist(live)
	if err := live.Pause("worker", "model-rejected", provenanceTestRejectReason); err != nil {
		t.Fatal(err)
	}
	persist(live)

	// The operator corrects the model in the agent's config while it is
	// parked: the config endpoint writes the agent overlay (paused: true).
	ac := cfg.Agents["worker"]
	ac.Model = "claude-sonnet-5-5"
	cfg.Agents["worker"] = ac
	if err := config.SaveAgentFile(agentsDir, "worker", ac); err != nil {
		t.Fatal(err)
	}
	persist(live)

	// Restart 1: the park comes back with its provenance, so a later model
	// change can still lift it.
	st := restart()
	if !st.Paused || st.PausedTrigger != "model-rejected" || st.PausedReason != provenanceTestRejectReason {
		t.Fatalf("after restart: paused=%v trigger=%q reason=%q, want the model-rejected park with its reason", st.Paused, st.PausedTrigger, st.PausedReason)
	}
	if st.PausedAt.IsZero() {
		t.Error("after restart: PausedAt lost")
	}

	// The operator resumes the agent: the runtime persists the resume through
	// the pause callback, and the next tick saves a hive-state that records
	// the agent running.
	if _, err := cfg.SetAgentPausedAndSave("worker", false); err != nil {
		t.Fatal(err)
	}
	resumed := cruxtest.NewRuntime(cfg.Agents, logger, crux.ProjectContext{})
	persist(resumed)
	if _, err := os.Stat(filepath.Join(agentsDir, "worker.yaml")); err != nil {
		t.Fatalf("overlay missing: %v", err)
	}

	// Restart 2: the resumed agent must stay resumed, not come back paused
	// with no provenance.
	st = restart()
	if st.Paused {
		t.Fatalf("after the resume and a restart: paused=%v trigger=%q reason=%q, want running (a pause with no provenance came back from the agent overlay)", st.Paused, st.PausedTrigger, st.PausedReason)
	}
}
