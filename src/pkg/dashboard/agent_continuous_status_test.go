package dashboard

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestAgentStatusCarriesContinuousBudgetAndCounters(t *testing.T) {
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "copilot", Enabled: true, Continuous: true},
		},
	}, Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "15m"}},
	}}}
	statuses := map[string]*crux.Process{
		"scanner": {Name: "scanner", Config: cfg.Agents["scanner"], State: crux.StateRunning},
	}
	govState := governor.State{
		Mode: governor.ModeIdle,
		Continuous: map[string]governor.ContinuousState{
			"scanner": {Blocked: "budget", Kicks: 3, TokensConsumed: 42},
		},
	}

	agents := buildAgents(statuses, cfg, govState)
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].ContinuousBlocked != "budget" {
		t.Fatalf("continuousBlocked = %q, want budget", agents[0].ContinuousBlocked)
	}
	if agents[0].ContinuousKicks != 3 || agents[0].ContinuousTokens != 42 {
		t.Fatalf("continuous counters = kicks %d tokens %d, want 3/42", agents[0].ContinuousKicks, agents[0].ContinuousTokens)
	}
	if want := []string{"idle", "busy", "surge"}; !reflect.DeepEqual(agents[0].ContinuousModes, want) {
		t.Fatalf("continuousModes = %v, want %v", agents[0].ContinuousModes, want)
	}
}

func TestAgentStatusContinuousNotInMode(t *testing.T) {
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"scanner": {Backend: "copilot", Enabled: true}},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"busy":  {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			"surge": {Cadences: map[string]config.Cadence{"scanner": "continuous"}},
		}},
	}
	statuses := map[string]*crux.Process{
		"scanner": {Name: "scanner", Config: cfg.Agents["scanner"], State: crux.StateRunning},
	}
	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeBusy})
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].Continuous {
		t.Fatal("busy mode must not report continuous for surge-only agent")
	}
	if agents[0].ContinuousBlocked != "not_in_mode" {
		t.Fatalf("continuousBlocked = %q, want not_in_mode", agents[0].ContinuousBlocked)
	}
}

func TestContinuousIdleSince(t *testing.T) {
	end := time.Date(2026, 10, 16, 20, 31, 0, 0, time.UTC)
	next := end.Add(time.Minute)
	if got := continuousIdleSince(true, "idle", end, next); !got.Equal(end) {
		t.Fatalf("idle continuous agent awaiting re-kick: got %v, want %v", got, end)
	}
	for name, got := range map[string]time.Time{
		"not continuous": continuousIdleSince(false, "idle", end, next),
		"working":        continuousIdleSince(true, "working", end, next),
		"no re-kick":     continuousIdleSince(true, "idle", end, time.Time{}),
		"never kicked":   continuousIdleSince(true, "idle", time.Time{}, next),
	} {
		if !got.IsZero() {
			t.Errorf("%s: want zero, got %v", name, got)
		}
	}
}
