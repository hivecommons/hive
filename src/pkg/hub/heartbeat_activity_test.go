package hub

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func activityTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func activityTestConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"busy": {Cadences: map[string]config.Cadence{"scanner": "1h"}},
			},
		},
	}
}

func TestAgentActivityForRidesPauseProvenance(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, activityTestLogger(), agent.ProjectContext{})
	pausedAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	lastPane := time.Date(2026, 8, 20, 8, 55, 0, 0, time.UTC)
	proc := &agent.AgentProcess{
		Paused:         true,
		PausedTrigger:  "dashboard-api",
		PausedReason:   "manual pause",
		PausedBy:       "owner",
		PausedAt:       pausedAt,
		NeedsLogin:     true,
		QuotaExhausted: true,
		LastPaneChange: lastPane,
	}

	act := AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)

	if !act.Paused || act.PausedTrigger != "dashboard-api" || act.PausedReason != "manual pause" ||
		act.PausedBy != "owner" || !act.PausedAt.Equal(pausedAt) {
		t.Errorf("pause provenance did not ride through verbatim: %+v", act)
	}
	if !act.NeedsLogin || !act.QuotaExhausted {
		t.Error("NeedsLogin/QuotaExhausted flags were dropped")
	}
	if !act.LastActivityAt.Equal(lastPane) {
		t.Errorf("LastActivityAt = %v, want %v", act.LastActivityAt, lastPane)
	}
	if !act.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v for a never-started process, want zero", act.StartedAt)
	}
	if act.Backend != "claude" {
		t.Errorf("Backend = %q, want claude", act.Backend)
	}
}

func TestAgentActivityForExpectedActiveHonorsOnDemandPack(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, activityTestLogger(), agent.ProjectContext{})
	proc := &agent.AgentProcess{}

	scheduled := AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)
	if !scheduled.ExpectedActive || !scheduled.Enabled {
		t.Fatalf("scheduled enabled agent got ExpectedActive=%v Enabled=%v", scheduled.ExpectedActive, scheduled.Enabled)
	}

	packOnDemand := AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, map[string]bool{"scanner": true})
	if packOnDemand.ExpectedActive {
		t.Error("pack-on-demand agent must never report ExpectedActive")
	}

	otherMode := AgentActivityFor(mgr, cfg, governor.State{}, "idle", "scanner", proc, nil)
	if otherMode.ExpectedActive {
		t.Error("agent with no cadence in current mode must not report ExpectedActive")
	}
}

func TestHeartbeatKickInterval(t *testing.T) {
	proc := &agent.AgentProcess{}
	govState := governor.State{Cadences: map[string]governor.AgentCadence{
		"quality": {Interval: 2 * time.Hour},
	}}
	if got := HeartbeatKickInterval(govState, "quality", proc, nil); got != 2*time.Hour {
		t.Errorf("HeartbeatKickInterval = %v, want 2h", got)
	}
	govState.Cadences["quality"] = governor.AgentCadence{Interval: 2 * time.Hour, Paused: true}
	if got := HeartbeatKickInterval(govState, "quality", proc, nil); got != 0 {
		t.Errorf("paused cadence interval = %v, want 0", got)
	}
}

func TestQuotaExhaustedCountsAndReasons(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"counted":   {State: agent.StateRunning, QuotaExhausted: true},
		"paused":    {State: agent.StateRunning, QuotaExhausted: true, Paused: true},
		"stopped":   {State: agent.StateStopped, QuotaExhausted: true},
		"has-quota": {State: agent.StateRunning},
	}
	if got := QuotaExhaustedProcessCount(statuses); got != 1 {
		t.Errorf("QuotaExhaustedProcessCount = %d, want 1", got)
	}
	agents := []AgentSummary{
		{Name: "guide", State: "running", QuotaExhausted: true},
		{Name: "scanner", State: "running", QuotaExhausted: true},
		{Name: "paused", State: "paused", Paused: true, QuotaExhausted: true},
		{Name: "supervisor"},
	}
	if got := QuotaExhaustedAgentCount(agents); got != 2 {
		t.Errorf("QuotaExhaustedAgentCount = %d, want 2", got)
	}
	if got := QuotaExhaustedAgentReason(0); got != "" {
		t.Errorf("QuotaExhaustedAgentReason(0) = %q, want empty", got)
	}
	if got := QuotaExhaustedAgentReason(3); got != "3 agent(s) out of provider quota" {
		t.Errorf("QuotaExhaustedAgentReason(3) = %q", got)
	}
}

func TestProviderLimitHeartbeatFields(t *testing.T) {
	reason, rebuffs := ProviderLimitHeartbeatFields([]AgentSummary{{State: "running", QuotaExhausted: true}}, nil)
	if rebuffs != 0 || reason != "1 agent(s) out of provider quota" {
		t.Fatalf("pane quota fallback = %q/%d", reason, rebuffs)
	}

	reason, rebuffs = ProviderLimitHeartbeatFields(nil, func() (string, time.Time, time.Time, int) {
		return "credit balance too low", time.Now(), time.Now(), 1
	})
	if rebuffs != 1 || reason != "provider spending limit reached — credit balance too low" {
		t.Fatalf("single rebuff = %q/%d", reason, rebuffs)
	}

	reason, rebuffs = ProviderLimitHeartbeatFields(nil, func() (string, time.Time, time.Time, int) {
		return "credit balance too low", time.Now(), time.Now(), 4
	})
	if rebuffs != 4 || reason != "provider spending limit reached — 4 refused calls: credit balance too low" {
		t.Fatalf("multi rebuff = %q/%d", reason, rebuffs)
	}
}
