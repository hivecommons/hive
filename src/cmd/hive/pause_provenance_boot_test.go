package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// #4041 boot-side provenance: the persisted actor must survive a restart, and
// the "restoring N paused agent(s)" boot line must (a) exclude agents that are
// startup-paused BY DESIGN (on-demand, e.g. brainstorm) and (b) break the
// remainder down by trigger — the inflated bare count is what made a
// deliberate owner quiesce read as a fresh systemic event on every upgrade
// restart of the frostyard fleet.

func TestRestoreAgentRuntimeState_ReplaysPausedBy(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Repos: []string{"r"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	statePath := filepath.Join(t.TempDir(), "hive-state.json")
	pausedAt := time.Date(2026, 8, 14, 16, 10, 10, 0, time.UTC)
	if err := snapshot.SaveState(statePath, &snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {
				Paused:        true,
				PausedAt:      &pausedAt,
				PausedTrigger: "dashboard-api",
				PausedReason:  "manual pause",
				PausedBy:      "bketelsen",
			},
		},
	}, restoreTestLogger()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	saved, err := snapshot.LoadState(statePath, restoreTestLogger())
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	m := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	restoreAgentRuntimeState(saved, cfg, m, restoreTestLogger())

	proc, err := m.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.PausedBy != "bketelsen" {
		t.Errorf("PausedBy = %q after restart, want %q (actor downgraded to anonymous on restore)", proc.PausedBy, "bketelsen")
	}
	if proc.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want %q", proc.PausedTrigger, "dashboard-api")
	}
	if !proc.PausedAt.Equal(pausedAt) {
		t.Errorf("PausedAt = %v, want %v", proc.PausedAt, pausedAt)
	}
}

func pausedProc(trigger string) *agent.AgentProcess {
	return &agent.AgentProcess{Paused: true, PausedTrigger: trigger}
}

// The frostyard shape: 8 owner-paused staff agents plus the always-startup-
// paused on-demand brainstorm. The old line said "restoring 9 paused
// agent(s)"; the new one must say 8, attribute them, and report the by-design
// one separately.
func TestPausedRestoreDetail_ExcludesByDesignOnDemand(t *testing.T) {
	enabled := map[string]config.AgentConfig{}
	statuses := map[string]*agent.AgentProcess{}
	staff := []string{"scanner", "quality", "ci-maintainer", "architect", "guide", "sec-check", "strategist", "supervisor"}
	for _, name := range staff {
		enabled[name] = config.AgentConfig{Enabled: true, Paused: true}
		statuses[name] = pausedProc("dashboard-api")
	}
	enabled["brainstorm"] = config.AgentConfig{Enabled: true, Paused: true, OnDemand: true}
	statuses["brainstorm"] = pausedProc("startup")
	enabled["running-agent"] = config.AgentConfig{Enabled: true}

	got := pausedRestoreDetail(enabled, nil, statuses)
	want := "restoring 8 paused agent(s) (dashboard-api: 8); 1 on-demand agent(s) startup-paused by design"
	if got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	if strings.Contains(got, "9") {
		t.Errorf("by-design startup-paused agent leaked into the headline count: %q", got)
	}
}

// An agent that is on-demand via its PACK definition (not its own config)
// must be excluded the same way.
func TestPausedRestoreDetail_ExcludesPackOnDemand(t *testing.T) {
	enabled := map[string]config.AgentConfig{
		"brainstorm": {Enabled: true, Paused: true},
		"scanner":    {Enabled: true, Paused: true},
	}
	statuses := map[string]*agent.AgentProcess{
		"brainstorm": pausedProc("startup"),
		"scanner":    pausedProc("dashboard-api"),
	}
	got := pausedRestoreDetail(enabled, map[string]bool{"brainstorm": true}, statuses)
	want := "restoring 1 paused agent(s) (dashboard-api: 1); 1 on-demand agent(s) startup-paused by design"
	if got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

// Mixed triggers appear sorted; an agent the manager has no provenance for
// still counts, under "unknown" — the total must always add up.
func TestPausedRestoreDetail_BreaksDownByTrigger(t *testing.T) {
	enabled := map[string]config.AgentConfig{
		"a": {Enabled: true, Paused: true},
		"b": {Enabled: true, Paused: true},
		"c": {Enabled: true, Paused: true},
		"d": {Enabled: true, Paused: true},
	}
	statuses := map[string]*agent.AgentProcess{
		"a": pausedProc("dashboard-api"),
		"b": pausedProc("dashboard-api"),
		"c": pausedProc("login-detector"),
		// "d" missing from statuses entirely.
	}
	got := pausedRestoreDetail(enabled, nil, statuses)
	want := "restoring 4 paused agent(s) (dashboard-api: 2, login-detector: 1, unknown: 1)"
	if got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}

func TestPausedRestoreDetail_NothingPaused(t *testing.T) {
	enabled := map[string]config.AgentConfig{
		"scanner": {Enabled: true},
	}
	got := pausedRestoreDetail(enabled, nil, map[string]*agent.AgentProcess{})
	want := "restoring 0 paused agent(s)"
	if got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
}
