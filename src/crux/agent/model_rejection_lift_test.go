package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// An operator's model or backend change must lift a model-rejection park, and
// only that pause. On the old code the change's restart went through Restart,
// which keeps every pause ("agent restart preserving paused state"), so the
// CLI never launched on the corrected model until someone resumed the agent
// by hand.

const liftTestRejectReason = "model claude-sonnet-5.5 rejected by the CLI 3 times in a row; set a valid model for this agent (the change resumes it)"

// pauseTriggersThatStay are the pauses a model change must never lift.
var pauseTriggersThatStay = []string{
	"dashboard-api",     // the operator's own pause
	"login-detector",    // a login prompt on screen
	RestartStormTrigger, // a restart loop
	DiskFullTrigger,     // a full disk
	BreakerTrigger,      // the fleet breaker
	"acmm-pack",         // an ACMM pack's pause
	"",                  // a pause restored from config with no provenance
}

func TestLiftModelRejectedPause_LiftsOnlyAModelRejectionPark(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"modelfix": {Backend: "claude", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	var mu sync.Mutex
	var persisted []bool
	m.SetPersistPauseCallback(func(name string, paused bool) {
		mu.Lock()
		persisted = append(persisted, paused)
		mu.Unlock()
	})
	if err := m.Pause("modelfix", ModelRejectedTrigger, liftTestRejectReason); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.agents["modelfix"].modelRejection = modelRejectionState{model: "claude-sonnet-5.5", count: modelRejectionParkAfter}
	m.mu.Unlock()

	lifted, err := m.liftModelRejectedPause("modelfix", "owner")
	if err != nil || !lifted {
		t.Fatalf("liftModelRejectedPause = (%v, %v), want (true, nil)", lifted, err)
	}
	st, _ := m.GetStatus("modelfix")
	if st.Paused || st.Config.Paused || st.State == StatePaused {
		t.Fatalf("after the lift: paused=%v config.paused=%v state=%s, want unpaused", st.Paused, st.Config.Paused, st.State)
	}
	if st.PausedTrigger != "" || st.PausedReason != "" || st.PausedBy != "" || !st.PausedAt.IsZero() {
		t.Errorf("pause provenance left behind: %+v", st)
	}
	m.mu.Lock()
	count := m.agents["modelfix"].modelRejection.count
	m.mu.Unlock()
	if count != 0 {
		t.Errorf("rejection count = %d after the lift, want a fresh count", count)
	}
	mu.Lock()
	got := append([]bool(nil), persisted...)
	mu.Unlock()
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Errorf("persisted pause states = %v, want [true false] (the lift is persisted like a resume)", got)
	}

	// A second call finds nothing to lift.
	if lifted, err := m.liftModelRejectedPause("modelfix", "owner"); err != nil || lifted {
		t.Fatalf("second lift = (%v, %v), want (false, nil)", lifted, err)
	}
	if _, err := m.liftModelRejectedPause("nobody", ""); err == nil {
		t.Fatal("lift of an unknown agent: want an error")
	}
}

func TestLiftModelRejectedPause_KeepsEveryOtherPause(t *testing.T) {
	for _, trigger := range pauseTriggersThatStay {
		t.Run("trigger="+trigger, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{
				"modelfix": {Backend: "claude", Model: "claude-sonnet-5-5"},
			}, discardLogger(), ProjectContext{})
			if err := m.PauseBy("modelfix", trigger, "paused for a reason of its own", "owner"); err != nil {
				t.Fatal(err)
			}
			if lifted, err := m.liftModelRejectedPause("modelfix", "owner"); err != nil || lifted {
				t.Fatalf("lift = (%v, %v), want (false, nil)", lifted, err)
			}
			st, _ := m.GetStatus("modelfix")
			if !st.Paused || st.PausedTrigger != trigger || st.PausedReason != "paused for a reason of its own" || st.PausedBy != "owner" {
				t.Fatalf("pause changed: paused=%v trigger=%q reason=%q by=%q", st.Paused, st.PausedTrigger, st.PausedReason, st.PausedBy)
			}
		})
	}
}

// End to end: the operator fixes the model of a parked agent, and the
// model-change restart launches the CLI.
func TestRestartAfterModelChange_ModelRejectedAgentLaunches(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"modelfix": {Backend: "claude", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "modelfix")
	defer cleanupAgent(t, m, "modelfix")
	if err := m.Pause("modelfix", ModelRejectedTrigger, liftTestRejectReason); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModelOverride("modelfix", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartAfterModelChange(context.Background(), "modelfix", "owner"); err != nil {
		t.Fatalf("RestartAfterModelChange: %v", err)
	}
	st, _ := m.GetStatus("modelfix")
	if st.Paused || st.State != StateRunning {
		t.Fatalf("after the model change: paused=%v state=%s, want a launched, unpaused agent", st.Paused, st.State)
	}
}

// A backend change is the same fix for the same park.
func TestRestartAfterModelChange_BackendChangeLaunchesModelRejectedAgent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"modelfix": {Backend: "copilot", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "modelfix")
	defer cleanupAgent(t, m, "modelfix")
	if err := m.Pause("modelfix", ModelRejectedTrigger, liftTestRejectReason); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBackendOverride("modelfix", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartAfterModelChange(context.Background(), "modelfix", "owner"); err != nil {
		t.Fatalf("RestartAfterModelChange: %v", err)
	}
	st, _ := m.GetStatus("modelfix")
	if st.Paused || st.State != StateRunning {
		t.Fatalf("after the backend change: paused=%v state=%s, want a launched, unpaused agent", st.Paused, st.State)
	}
}

// An operator's own pause survives the model change's restart, as it always
// did through Restart.
func TestRestartAfterModelChange_OperatorPauseStays(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"modelfix": {Backend: "claude", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "modelfix")
	defer cleanupAgent(t, m, "modelfix")
	if err := m.PauseBy("modelfix", "dashboard-api", "manual pause", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModelOverride("modelfix", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if err := m.RestartAfterModelChange(context.Background(), "modelfix", "owner"); err != nil {
		t.Fatalf("RestartAfterModelChange: %v", err)
	}
	st, _ := m.GetStatus("modelfix")
	if !st.Paused || st.State != StatePaused || st.PausedTrigger != "dashboard-api" {
		t.Fatalf("operator pause: paused=%v state=%s trigger=%q, want it kept", st.Paused, st.State, st.PausedTrigger)
	}
}
