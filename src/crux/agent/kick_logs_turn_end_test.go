package agent

// Tests for archiving a kick log when the kicked TURN ENDS, not only at the
// next rotation point. Observed on a hosted hive: a daily-cadence scanner's
// turn ended 22s after the kick, the container was then killed hours later
// (exit code 17, not a SIGTERM), and the run's only copy — the tmux
// scrollback — died with it. The turn-end snapshot closes that window while
// keeping one archive per kick: the final rotation-point snapshot of the same
// kick supersedes the turn-end file rather than sitting beside it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// turnEndTestManager is kickLogTestManager plus a delivered kick whose turn
// has not been classified yet.
func turnEndTestManager(t *testing.T, captured string) (*Manager, *AgentProcess, string, time.Time) {
	t.Helper()
	m, agent, dir := kickLogTestManager(t, captured)
	kickAt := time.Date(2026, 10, 28, 11, 1, 31, 0, time.UTC)
	agent.LastKick = &kickAt
	agent.kickLogPending = true
	return m, agent, filepath.Join(dir, "scanner"), kickAt
}

func listArchiveNames(t *testing.T, m *Manager) []string {
	t.Helper()
	infos, err := m.ListKickLogs("scanner")
	if err != nil {
		t.Fatalf("ListKickLogs: %v", err)
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.ID)
	}
	return names
}

func TestSettleKickOutcome_ArchivesOnTurnEndWithoutClearing(t *testing.T) {
	m, agent, agentDir, kickAt := turnEndTestManager(t, "scanner run output\n")
	cleared := false
	termSeams(m).clearHistory = func(*AgentProcess) { cleared = true }

	m.mu.Lock()
	_, settled := m.settleKickOutcomeLocked(agent, productivePane, kickAt.Add(22*time.Second))
	m.mu.Unlock()
	if !settled {
		t.Fatal("productive idle pane did not settle the turn")
	}

	names := listArchiveNames(t, m)
	if len(names) != 1 {
		t.Fatalf("archives = %v, want exactly one turn-end archive", names)
	}
	wantStem := kickAt.UTC().Format(kickLogTimestampFormat)
	if !strings.HasPrefix(names[0], wantStem) || kickLogReason(names[0]) != kickLogReasonTurnEnd {
		t.Errorf("archive = %q, want %s-%s.log (named for the kick, reason turn-end)", names[0], wantStem, kickLogReasonTurnEnd)
	}
	data, err := os.ReadFile(filepath.Join(agentDir, names[0]))
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	if !strings.Contains(string(data), "reason: turn-end") || !strings.Contains(string(data), "scanner run output") {
		t.Errorf("archive body missing header reason or content:\n%s", data)
	}
	if cleared {
		t.Error("turn-end archive cleared the scrollback; the pane must keep showing the result until the next kick")
	}
	if !agent.kickLogPending {
		t.Error("kickLogPending cleared by the turn-end archive; the next rotation point must still take the final snapshot")
	}
	if !agent.kickLogArchivedKick.Equal(kickAt) || filepath.Base(agent.kickLogArchivedPath) != names[0] {
		t.Errorf("archived marker = (%v, %q), want (%v, %q)", agent.kickLogArchivedKick, agent.kickLogArchivedPath, kickAt, names[0])
	}
}

// A turn-end archive is skipped when nothing is pending (fresh session, or the
// output was already rotated out) — the boot banner is not a kick log.
func TestArchiveKickLogOnTurnEnd_NothingPending(t *testing.T) {
	m, agent, _, _ := turnEndTestManager(t, "boot banner")
	agent.kickLogPending = false

	m.mu.Lock()
	wrote := m.archiveKickLogOnTurnEndLocked(agent)
	m.mu.Unlock()
	if wrote {
		t.Fatal("archived with nothing pending")
	}
	if names := listArchiveNames(t, m); len(names) != 0 {
		t.Errorf("archives = %v, want none", names)
	}
}

// A second turn-end observation for the same kick (the agent kept producing
// after the first idle prompt, or hive restarted and re-classified) must
// overwrite the same file with the newer content, not add a duplicate.
func TestArchiveKickLogOnTurnEnd_SameKickOverwrites(t *testing.T) {
	m, agent, agentDir, _ := turnEndTestManager(t, "first output\n")

	m.mu.Lock()
	first := m.archiveKickLogOnTurnEndLocked(agent)
	termSeams(m).captureFullLog = func(*AgentProcess) (string, error) { return "first output\nmore output\n", nil }
	second := m.archiveKickLogOnTurnEndLocked(agent)
	m.mu.Unlock()
	if !first || !second {
		t.Fatalf("archive results = %v, %v, want both written", first, second)
	}

	names := listArchiveNames(t, m)
	if len(names) != 1 {
		t.Fatalf("archives = %v, want one file for one kick", names)
	}
	data, _ := os.ReadFile(filepath.Join(agentDir, names[0]))
	if !strings.Contains(string(data), "more output") {
		t.Errorf("later turn-end content did not win:\n%s", data)
	}
}

// The next kick's rotation after a turn-end archive: the final snapshot
// supersedes the turn-end file (one archive for the kick, reason "kick",
// latest content), and the scrollback is cleared exactly as before.
func TestRotateOnKick_SupersedesTurnEndArchive(t *testing.T) {
	m, agent, agentDir, kickAt := turnEndTestManager(t, "run output\n")
	cleared := 0
	termSeams(m).clearHistory = func(*AgentProcess) { cleared++ }

	m.mu.Lock()
	m.archiveKickLogOnTurnEndLocked(agent)
	termSeams(m).captureFullLog = func(*AgentProcess) (string, error) { return "run output\ntrailing hook output\n", nil }
	m.rotateKickLogOnKickLocked(agent)
	m.mu.Unlock()

	names := listArchiveNames(t, m)
	if len(names) != 1 {
		t.Fatalf("archives = %v, want the turn-end file replaced by one kick archive", names)
	}
	if kickLogReason(names[0]) != "kick" || !strings.HasPrefix(names[0], kickAt.UTC().Format(kickLogTimestampFormat)) {
		t.Errorf("archive = %q, want <kick timestamp>-kick.log", names[0])
	}
	data, _ := os.ReadFile(filepath.Join(agentDir, names[0]))
	if !strings.Contains(string(data), "trailing hook output") {
		t.Errorf("final rotation content did not win:\n%s", data)
	}
	if cleared != 1 {
		t.Errorf("clearHistory calls = %d, want 1 (rotation still clears for the next kick)", cleared)
	}
	if agent.kickLogPending {
		t.Error("kickLogPending still set after rotation")
	}
}

// Distinct kicks never supersede each other: the marker is keyed by LastKick,
// so the previous kick's archive stays when a later kick is archived.
func TestArchiveKickLog_DifferentKicksKeepBothArchives(t *testing.T) {
	m, agent, _, kickAt := turnEndTestManager(t, "run one\n")

	m.mu.Lock()
	m.archiveKickLogOnTurnEndLocked(agent)
	next := kickAt.Add(24 * time.Hour)
	agent.LastKick = &next
	agent.kickLogPending = true
	m.archiveKickLogOnTurnEndLocked(agent)
	m.mu.Unlock()

	if names := listArchiveNames(t, m); len(names) != 2 {
		t.Fatalf("archives = %v, want one per kick", names)
	}
}

// Retention counts kicks, not files: with retention 2 and a turn-end archive
// superseded by its own rotation, the two previous kicks' archives survive
// and the newly written file is never pruned.
func TestPruneAfterSupersede_KeepsRetentionOfDistinctKicks(t *testing.T) {
	m, agent, _, kickAt := turnEndTestManager(t, "output\n")
	m.kickLogRetention = 2

	m.mu.Lock()
	for i := 0; i < 2; i++ {
		k := kickAt.Add(time.Duration(i) * time.Hour)
		agent.LastKick = &k
		agent.kickLogPending = true
		m.archiveKickLogOnTurnEndLocked(agent)
		m.rotateKickLogOnKickLocked(agent)
	}
	m.mu.Unlock()

	names := listArchiveNames(t, m)
	if len(names) != 2 {
		t.Fatalf("archives = %v, want 2 (one per kick, retention 2)", names)
	}
	for _, n := range names {
		if kickLogReason(n) != "kick" {
			t.Errorf("archive %q should have been superseded by its kick rotation", n)
		}
	}
	if agent.kickLogArchivedPath == "" {
		t.Fatal("no archived marker recorded")
	}
	if _, err := os.Stat(agent.kickLogArchivedPath); err != nil {
		t.Errorf("newest archive %q pruned: %v", agent.kickLogArchivedPath, err)
	}
}

// Restart/shutdown after a turn ended normally: the final snapshot still
// supersedes the turn-end archive, but the turn is NOT counted as interrupted
// (nothing was lost) and the restart/kick breaker is not armed.
func TestTearDown_SettledTurnIsArchivedButNotInterrupted(t *testing.T) {
	m, agent, _, kickAt := turnEndTestManager(t, "run output\n")
	agent.LastPaneChange = kickAt.Add(10 * time.Second)

	m.mu.Lock()
	m.settleKickOutcomeLocked(agent, productivePane, kickAt.Add(22*time.Second))
	interrupted, producing := m.tearDownTurnLocked(agent, "shutdown")
	m.mu.Unlock()

	if interrupted || producing {
		t.Errorf("tearDown = (%v, %v) for a turn that had already ended, want (false, false)", interrupted, producing)
	}
	if agent.TurnLoss.Interruptions != 0 || agent.TurnLoss.Bytes != 0 {
		t.Errorf("TurnLoss = %+v, want nothing recorded for an already-archived, ended turn", agent.TurnLoss)
	}
	if agent.kickLogPending {
		t.Error("kickLogPending still set after teardown")
	}
	names := listArchiveNames(t, m)
	if len(names) != 1 || kickLogReason(names[0]) != "shutdown" {
		t.Errorf("archives = %v, want the turn-end file superseded by one shutdown archive", names)
	}
}

// The crash path is unchanged: a turn that never settled and is torn down is
// still recorded as interrupted with its archived bytes.
func TestTearDown_UnsettledTurnStillCountsAsInterrupted(t *testing.T) {
	m, agent, _, kickAt := turnEndTestManager(t, "partial output\n")
	agent.LastPaneChange = kickAt.Add(10 * time.Second)

	m.mu.Lock()
	interrupted, producing := m.tearDownTurnLocked(agent, "restart")
	m.mu.Unlock()

	if !interrupted || !producing {
		t.Errorf("tearDown = (%v, %v), want (true, true)", interrupted, producing)
	}
	if agent.TurnLoss.Interruptions != 1 || agent.TurnLoss.Bytes != int64(len("partial output\n")) {
		t.Errorf("TurnLoss = %+v, want one interruption with the archived bytes", agent.TurnLoss)
	}
	if names := listArchiveNames(t, m); len(names) != 1 || kickLogReason(names[0]) != "restart" {
		t.Errorf("archives = %v, want one restart archive", names)
	}
}

// Removing a superseded file that is already gone (operator cleanup, prune on
// another path) is not an error and does not stop the new archive.
func TestArchiveKickLog_SupersededFileAlreadyGone(t *testing.T) {
	m, agent, _, _ := turnEndTestManager(t, "output\n")

	m.mu.Lock()
	m.archiveKickLogOnTurnEndLocked(agent)
	if err := os.Remove(agent.kickLogArchivedPath); err != nil {
		t.Fatalf("removing turn-end archive: %v", err)
	}
	wrote := m.archiveKickLogLocked(agent, "kick")
	m.mu.Unlock()

	if !wrote {
		t.Fatal("final archive not written after the superseded file vanished")
	}
	if names := listArchiveNames(t, m); len(names) != 1 || kickLogReason(names[0]) != "kick" {
		t.Errorf("archives = %v, want one kick archive", names)
	}
}
