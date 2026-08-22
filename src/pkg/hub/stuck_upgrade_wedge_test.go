package hub

// Regression tests for the "My Hives rows stuck in Upgrading" wedge (the
// devx-gabriel/drools/z-aiops cohorts): three state-machine gaps that let a
// latched upgrade spin forever instead of resolving to done or an honest
// failure.
//
//  1. Every ordinary heartbeat rebuilt the registry entry from the payload and
//     silently reset OrphanedUpgradeSweeps to 0, so the terminal "give up
//     after maxOrphanedUpgradeSweeps and report UpgradeFailed" state was
//     unreachable for any hive that heartbeats — i.e. exactly the hives the
//     sweep evaluates. The fleet looped clear→re-arm→re-latch forever.
//  2. A spoke that died mid-upgrade and never heartbeated again could never be
//     swept (the liveness evidence requires a beat AFTER the instruction), so
//     its row spun "Upgrading" without bound — 1.9h and counting on the
//     z-aiops cohort. silentUpgradeDeadline now bounds that.
//  3. While spoke upgrades were paused the sweep was suppressed entirely, so a
//     CONVERGED latch (upgrade already landed, only the flag stale) also sat
//     spinning for the whole freeze even though clearing it delivers nothing.

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// wedgeHive seeds newHeartbeatHub's registry with one latched, wedged hive.
func wedgeHive(s *HubServer, sweeps int) {
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "old0000",
		Upgrading: true, UpgradeTarget: "fc32ae4",
		UpgradeStartedAt:      time.Now().Add(-5 * time.Minute),
		LastHeartbeat:         time.Now().Format(time.RFC3339),
		OrphanedUpgradeSweeps: sweeps,
	}}
}

// TestHeartbeatPreservesOrphanSweepBudget is the gap-1 regression: an ordinary
// beat from a hive still wedged on its old SHA must NOT reset the retry budget
// the sweep has accrued against it.
func TestHeartbeatPreservesOrphanSweepBudget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	s := newHeartbeatHub()
	wedgeHive(s, 2)

	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"old0000","git_branch":"v2","upgrading":false}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if !entry.Upgrading {
		t.Fatalf("still-wedged hive must stay latched, got %+v", entry)
	}
	if entry.OrphanedUpgradeSweeps != 2 {
		t.Fatalf("OrphanedUpgradeSweeps = %d after an ordinary beat, want 2 — "+
			"a beat-reset budget makes the terminal UpgradeFailed state unreachable",
			entry.OrphanedUpgradeSweeps)
	}
}

// TestHeartbeatResetsBudgetWhenBuildMoves: the moment the hive demonstrably
// lands on a NEW build, any accrued budget is spent history and must not leak
// into the next upgrade.
func TestHeartbeatResetsBudgetWhenBuildMoves(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	s := newHeartbeatHub()
	wedgeHive(s, 2)

	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"fc32ae4","git_branch":"v2","upgrading":false}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if entry.Upgrading || entry.UpgradeTarget != "" {
		t.Fatalf("upgrade landed at the target — latch must clear, got %+v", entry)
	}
	if entry.OrphanedUpgradeSweeps != 0 {
		t.Fatalf("OrphanedUpgradeSweeps = %d after landing, want 0", entry.OrphanedUpgradeSweeps)
	}
}

// TestSweepEscalationSurvivesHeartbeats drives the full loop the fleet was
// stuck in: sweep, ordinary beat, re-wedge — three times. With the budget
// carried across beats, the third sweep must go terminal (honest
// UpgradeFailed) instead of looping forever.
func TestSweepEscalationSurvivesHeartbeats(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	s := newHeartbeatHub()
	wedgeHive(s, 0)

	rewedge := func() {
		s.mu.Lock()
		h := &s.registry.Hives[0]
		h.Upgrading = true
		h.UpgradeTarget = "fc32ae4"
		h.UpgradeStartedAt = time.Now().Add(-orphanedUpgradeClearAfter - time.Minute)
		h.LastHeartbeat = time.Now().Format(time.RFC3339)
		s.mu.Unlock()
	}

	for i := 1; i <= maxOrphanedUpgradeSweeps; i++ {
		rewedge()
		s.sweepOrphanedUpgrades()
		rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"old0000","git_branch":"v2","upgrading":false}`)
		if rec.Code != 200 {
			t.Fatalf("beat %d: status = %d body=%s", i, rec.Code, rec.Body.String())
		}
		s.mu.RLock()
		entry := s.registry.Hives[0]
		s.mu.RUnlock()
		if entry.OrphanedUpgradeSweeps != i {
			t.Fatalf("after sweep %d + beat: budget = %d, want %d", i, entry.OrphanedUpgradeSweeps, i)
		}
	}

	s.mu.RLock()
	entry := s.registry.Hives[0]
	_, armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if !entry.UpgradeFailed {
		t.Fatalf("after %d sweeps the hub must give up and report a fault, got %+v",
			maxOrphanedUpgradeSweeps, entry)
	}
	if entry.UpgradeError == "" || !strings.Contains(entry.UpgradeError, "never landed") {
		t.Fatalf("terminal fault must carry an honest cause, got %q", entry.UpgradeError)
	}
	if armed {
		t.Fatal("a terminal fault must drop the armed instruction, not keep re-delivering it")
	}
}

// TestSweepClearsSilentDeadSpoke is the gap-2 regression (the z-aiops cohort):
// a spoke silent since the instruction, past silentUpgradeDeadline, is swept
// with an honest reason instead of spinning forever.
func TestSweepClearsSilentDeadSpoke(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	s := &HubServer{logger: slog.Default(), saveCh: make(chan struct{}, 1), heartbeatUpgrade: make(map[string]string)}
	started := time.Now().Add(-silentUpgradeDeadline - time.Minute)
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "old0000",
		Upgrading: true, UpgradeTarget: "fc32ae4",
		UpgradeStartedAt: started,
		LastHeartbeat:    started.Add(-time.Minute).Format(time.RFC3339),
	}}

	ev := evaluateOrphanedUpgrade(&s.registry.Hives[0], time.Now(), "")
	if !ev.orphaned || !strings.Contains(ev.reason, "silent") {
		t.Fatalf("silent-past-deadline verdict = %+v, want orphaned with a reason naming the silence", ev)
	}

	s.sweepOrphanedUpgrades()
	s.mu.RLock()
	entry := s.registry.Hives[0]
	armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if entry.Upgrading {
		t.Fatalf("dead-spoke latch must be cleared, got %+v", entry)
	}
	if entry.OrphanedUpgradeSweeps != 1 {
		t.Fatalf("sweeps = %d, want 1", entry.OrphanedUpgradeSweeps)
	}
	if armed != "fc32ae4" {
		t.Fatalf("target must be re-armed for the spoke's next check-in, got %q", armed)
	}
}

// TestSweepUnderSilentDeadlineLeavesLatch: short of the deadline, silence is
// still treated as a restart in flight — the pre-existing safety.
func TestSweepUnderSilentDeadlineLeavesLatch(t *testing.T) {
	started := time.Now().Add(-silentUpgradeDeadline + 5*time.Minute)
	entry := RegistryEntry{
		ID: "h1", GitHash: "old0000",
		Upgrading: true, UpgradeTarget: "fc32ae4",
		UpgradeStartedAt: started,
		LastHeartbeat:    started.Add(-time.Minute).Format(time.RFC3339),
	}
	if ev := evaluateOrphanedUpgrade(&entry, time.Now(), ""); ev.orphaned {
		t.Fatalf("silent but under deadline must not be cleared, got %+v", ev)
	}
}

// TestPausedSweepStillClearsConvergedLatch is the gap-3 regression: the pause
// suppresses delivery and budget spend, not truth-telling. A latch whose
// upgrade already LANDED is cleared even while paused; an abandoned orphan
// still waits untouched (TestSpokePauseBlocksOrphanSweep pins that half).
func TestPausedSweepStillClearsConvergedLatch(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetSHACaches(t)
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	s := &HubServer{logger: slog.Default(), saveCh: make(chan struct{}, 1), heartbeatUpgrade: make(map[string]string)}
	s.registry.Hives = []RegistryEntry{
		{
			// Converged: alive, past the clear threshold, already ON the target.
			ID: "done1", GitBranch: "v2", GitHash: "fc32ae4",
			Upgrading: true, UpgradeTarget: "fc32ae4",
			UpgradeStartedAt: time.Now().Add(-orphanedUpgradeClearAfter - time.Minute),
			LastHeartbeat:    time.Now().Format(time.RFC3339),
		},
		{
			// Abandoned orphan: alive but still on the old SHA.
			ID: "wedge1", GitBranch: "v2", GitHash: "old0000",
			Upgrading: true, UpgradeTarget: "fc32ae4",
			UpgradeStartedAt: time.Now().Add(-orphanedUpgradeClearAfter - time.Minute),
			LastHeartbeat:    time.Now().Format(time.RFC3339),
		},
	}
	pauseSpokes(t, s, true)

	s.sweepOrphanedUpgrades()
	s.mu.RLock()
	done, wedge := s.registry.Hives[0], s.registry.Hives[1]
	_, armed := s.heartbeatUpgrade["wedge1"]
	s.mu.RUnlock()
	if done.Upgrading || done.UpgradeTarget != "" || done.OrphanedUpgradeSweeps != 0 {
		t.Fatalf("paused: converged latch must still clear (no delivery, no budget), got %+v", done)
	}
	if !wedge.Upgrading || wedge.OrphanedUpgradeSweeps != 0 || armed {
		t.Fatalf("paused: abandoned orphan must wait untouched, got %+v armed=%v", wedge, armed)
	}
}
