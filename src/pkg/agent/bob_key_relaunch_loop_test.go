package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// These tests cover the relaunch LOOP body of RelaunchBobAgentsAwaitingKey —
// the kill-stale-session, clear-backoff, and Start dispatch steps — which the
// selection/key-gate tests in bob_key_relaunch_test.go deliberately stop short
// of. The loop is where the #5958 backoff-clear and the one-failure-must-not-
// abort-the-fleet semantics live.

// parkAgentForMissingKeyLocked puts a NewManager-built agent into exactly the
// state launchInTmux leaves a bob agent in when no API key is configured,
// including the start-failure record that recordStartFailureLocked writes.
func parkAgentForMissingKeyLocked(m *Manager, name, sentinelReason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent := m.agents[name]
	agent.State = StateFailed
	agent.awaitingBobKey = true
	agent.StartFailureClass = string(StartFailureCredentialMissing)
	agent.StartFailureReason = sentinelReason
	agent.StartFailureCount = 3
	// A live backoff window: without the clearStartFailureLocked call in the
	// relaunch loop, this alone must not gate the retry (#5958), but the loop
	// clears it so the record cannot go stale either way.
	agent.StartBackoffUntil = time.Now().Add(time.Hour)
}

// TestRelaunchBobAgentsAwaitingKeyRelaunchLoop drives one parked bob agent all
// the way through the loop body: the stale tmux session is killed, the start-
// failure backoff is cleared before Start, and a Start that does not error is
// reported as relaunched.
func TestRelaunchBobAgentsAwaitingKeyRelaunchLoop(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: bobBackend},
	}, discardLogger(), ProjectContext{})
	m.SetBobAPIKeyResolver(func() string { return "resolved-test-key" })

	const sentinel = "sentinel: parked before key save"
	parkAgentForMissingKeyLocked(m, "scanner", sentinel)

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	// A stale key-less session must exist so the loop's kill-session step has
	// something real to tear down (the load-bearing half of the fix: the old
	// pane's shell can never learn the key).
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()

	got := m.RelaunchBobAgentsAwaitingKey(context.Background())
	defer cleanupAgent(t, m, "scanner")

	// Start's park-and-return branches deliberately return nil (one agent must
	// never abort the fleet), so with or without a bob binary on PATH the
	// relaunch itself must be reported.
	if len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("RelaunchBobAgentsAwaitingKey() = %v, want [scanner]", got)
	}

	// The pre-save failure record must be gone. If the launch attempt failed
	// again for a NEW reason (e.g. no bob binary in this environment) a fresh
	// record may exist — but never the sentinel one, which would mean the
	// backoff from the missing-key era survived the key save (#5958).
	m.mu.RLock()
	reason := agent.StartFailureReason
	m.mu.RUnlock()
	if reason == sentinel {
		t.Errorf("StartFailureReason = %q still the pre-save record; clearStartFailureLocked was not applied before Start", reason)
	}
}

// TestRelaunchBobAgentsAwaitingKeyStartErrorContinues pins the fleet-safety
// rule: when Start errors for one candidate the loop logs, skips it, and still
// relaunches the rest — one agent's failure never aborts the fleet.
func TestRelaunchBobAgentsAwaitingKeyStartErrorContinues(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"stuck":   {Backend: bobBackend},
		"scanner": {Backend: bobBackend},
	}, discardLogger(), ProjectContext{})
	m.SetBobAPIKeyResolver(func() string { return "resolved-test-key" })

	parkAgentForMissingKeyLocked(m, "stuck", "parked")
	parkAgentForMissingKeyLocked(m, "scanner", "parked")

	// A claimed launch guard makes Start return an error ("launch already in
	// progress") without any tmux side effects — the cheapest deterministic
	// Start failure available through the real code path.
	m.mu.Lock()
	m.agents["stuck"].launching = true
	m.mu.Unlock()

	got := m.RelaunchBobAgentsAwaitingKey(context.Background())
	defer cleanupAgent(t, m, "scanner")
	defer cleanupAgent(t, m, "stuck")

	if len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("RelaunchBobAgentsAwaitingKey() = %v, want [scanner] — the stuck agent's error must not abort the fleet", got)
	}

	// The failed candidate must remain parked and eligible for a later save.
	m.mu.RLock()
	stillParked := m.agents["stuck"].awaitingBobKey
	m.mu.RUnlock()
	if !stillParked {
		t.Error("stuck agent lost awaitingBobKey without a successful launch; a later key save would miss it")
	}
}
