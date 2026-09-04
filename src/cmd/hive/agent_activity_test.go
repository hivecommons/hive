package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
)

// activityTestConfig builds the minimal config agentActivityFor needs: one
// enabled scanner agent scheduled on a kicking cadence in the "busy" mode.
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

// agentActivityFor must carry the #4041 pause provenance (WHO/WHY/WHEN) to the
// hub verbatim — dropping any of these is what turned a deliberate owner
// quiesce into a multi-day incident investigation.
func TestAgentActivityForRidesPauseProvenance(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
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

	act := hub.AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)

	if !act.Paused || act.PausedTrigger != "dashboard-api" || act.PausedReason != "manual pause" ||
		act.PausedBy != "owner" || !act.PausedAt.Equal(pausedAt) {
		t.Errorf("pause provenance did not ride through verbatim: %+v", act)
	}
	if !act.NeedsLogin || !act.QuotaExhausted {
		t.Error("NeedsLogin/QuotaExhausted flags were dropped")
	}
	if !act.LastActivityAt.Equal(lastPane) {
		t.Errorf("LastActivityAt = %v, want the pane-change time %v", act.LastActivityAt, lastPane)
	}
	// StartedAt is nil on the process: the activity must report a ZERO time
	// (hub reads it as "unknown"), never a fabricated timestamp.
	if !act.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v for a never-started process, want zero", act.StartedAt)
	}
	// Backend rides along so the hub can interpret NeedsLogin.
	if act.Backend != "claude" {
		t.Errorf("Backend = %q, want %q", act.Backend, "claude")
	}
}

// The EXPECTED leg: a cadence-scheduled agent in the current mode reports
// ExpectedActive; the same agent flagged on-demand by an ACMM pack must NOT —
// on-demand agents are never on a schedule, and reporting them as expected
// would make every idle on-demand agent read as divergent fleet-side.
func TestAgentActivityForExpectedActiveHonorsOnDemandPack(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	proc := &agent.AgentProcess{}

	scheduled := hub.AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)
	if !scheduled.ExpectedActive {
		t.Error("cadence-scheduled agent in the current mode should report ExpectedActive")
	}
	if !scheduled.Enabled {
		t.Error("enabled agent should report Enabled")
	}

	packOnDemand := hub.AgentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc,
		map[string]bool{"scanner": true})
	if packOnDemand.ExpectedActive {
		t.Error("pack-on-demand agent must never report ExpectedActive")
	}

	otherMode := hub.AgentActivityFor(mgr, cfg, governor.State{}, "idle", "scanner", proc, nil)
	if otherMode.ExpectedActive {
		t.Error("agent with no cadence in the current mode must not report ExpectedActive")
	}
}

// A nil config must leave the EXPECTED leg conservatively false rather than
// panicking or guessing — the hub reads all-false as UNKNOWN, never as a
// divergence verdict.
func TestAgentActivityForNilConfig(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	act := hub.AgentActivityFor(mgr, nil, governor.State{}, "busy", "scanner", &agent.AgentProcess{}, nil)
	if act.ExpectedActive || act.Enabled {
		t.Errorf("nil config: ExpectedActive=%v Enabled=%v, want both false", act.ExpectedActive, act.Enabled)
	}
}

// The ABLE leg: an agent the manager does not know must leave all three
// capability bits false (hub-side UNKNOWN) and carry no backend — inventing
// capabilities for an unknown agent would fabricate a divergence signal.
func TestAgentActivityForUnknownAgentCapabilitiesStayFalse(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	act := hub.AgentActivityFor(mgr, cfg, governor.State{}, "busy", "ghost", &agent.AgentProcess{}, nil)
	if act.CanOpenIssue || act.CanOpenPR || act.CanMerge {
		t.Errorf("unknown agent capabilities = (%v, %v, %v), want all false (UNKNOWN)",
			act.CanOpenIssue, act.CanOpenPR, act.CanMerge)
	}
	if act.Backend != "" {
		t.Errorf("unknown agent Backend = %q, want empty", act.Backend)
	}
}
