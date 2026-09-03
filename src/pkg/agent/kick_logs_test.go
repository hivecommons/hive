package agent

// Tests for durable per-kick agent run logs (#4296, #4295): the archive
// write, the retention pruning, the rotation-on-kick ordering (snapshot then
// clear then input — never input first), the restart hook (snapshot happens
// even when the relaunch itself fails), and the list/read accessors the
// dashboard endpoints sit on.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// kickLogTestManager builds a Manager with one agent whose tmux capture and
// clear-history are seamed, archiving into a fresh temp dir.
func kickLogTestManager(t *testing.T, captured string) (*Manager, *AgentProcess, string) {
	t.Helper()
	dir := t.TempDir()
	agent := &AgentProcess{
		Name:        "scanner",
		Config:      config.AgentConfig{Backend: "claude"},
		State:       StateRunning,
		tmuxSession: "hive-scanner-kicklog-test",
	}
	m := &Manager{
		agents:           map[string]*AgentProcess{"scanner": agent},
		idToName:         map[string]string{"scanner": "scanner"},
		logger:           slog.Default(),
		kickLogDir:       dir,
		kickLogRetention: defaultKickLogRetention,
		kickLogMaxBytes:  defaultKickLogMaxBytes,
		terminal: funcTerminal{
			captureFullLog: func(*AgentProcess) (string, error) { return captured, nil },
			clearHistory:   func(*AgentProcess) {},
		},
	}
	return m, agent, dir
}

func TestKickLogSettingsFromEnv_Defaults(t *testing.T) {
	t.Setenv(kickLogDirEnv, "")
	t.Setenv(kickLogRetentionEnv, "")
	t.Setenv(kickLogMaxBytesEnv, "")
	dir, retention, maxBytes := kickLogSettingsFromEnv()
	if dir != defaultKickLogDir {
		t.Errorf("dir = %q, want %q", dir, defaultKickLogDir)
	}
	if retention != defaultKickLogRetention {
		t.Errorf("retention = %d, want %d", retention, defaultKickLogRetention)
	}
	if maxBytes != defaultKickLogMaxBytes {
		t.Errorf("maxBytes = %d, want %d", maxBytes, defaultKickLogMaxBytes)
	}
}

func TestKickLogSettingsFromEnv_Overrides(t *testing.T) {
	t.Setenv(kickLogDirEnv, "/elsewhere/kicks")
	t.Setenv(kickLogRetentionEnv, "3")
	t.Setenv(kickLogMaxBytesEnv, "1024")
	dir, retention, maxBytes := kickLogSettingsFromEnv()
	if dir != "/elsewhere/kicks" || retention != 3 || maxBytes != 1024 {
		t.Errorf("got (%q, %d, %d), want (/elsewhere/kicks, 3, 1024)", dir, retention, maxBytes)
	}
}

// Garbage or negative overrides must fall back to the defaults rather than
// disable archiving by accident; an explicit "0" retention IS honored as off.
func TestKickLogSettingsFromEnv_InvalidOverrides(t *testing.T) {
	t.Setenv(kickLogRetentionEnv, "not-a-number")
	t.Setenv(kickLogMaxBytesEnv, "-5")
	_, retention, maxBytes := kickLogSettingsFromEnv()
	if retention != defaultKickLogRetention || maxBytes != defaultKickLogMaxBytes {
		t.Errorf("got (%d, %d), want defaults", retention, maxBytes)
	}

	t.Setenv(kickLogRetentionEnv, "0")
	_, retention, _ = kickLogSettingsFromEnv()
	if retention != 0 {
		t.Errorf("retention = %d, want explicit 0 honored", retention)
	}
}

func TestArchiveKickLogLocked_WritesHeaderAndContent(t *testing.T) {
	m, agent, dir := kickLogTestManager(t, "run output line 1\nrun output line 2\n")
	kickAt := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	agent.LastKick = &kickAt
	agent.LastKickMessage = "scan the repo\nand report"

	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archiveKickLogLocked returned false, want an archive written")
	}

	infos, err := m.ListKickLogs("scanner")
	if err != nil {
		t.Fatalf("ListKickLogs: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("archives = %d, want 1", len(infos))
	}
	if infos[0].Reason != "kick" {
		t.Errorf("reason = %q, want kick", infos[0].Reason)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scanner", infos[0].ID))
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"agent: scanner",
		"reason: kick",
		"kick_started: 2026-08-20T07:00:00Z",
		"kick_prompt: scan the repo and report", // newline flattened
		"run output line 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("archive missing %q:\n%s", want, body)
		}
	}
}

// Nothing worth archiving — an empty or whitespace-only capture — must not
// burn a retention slot with an empty file.
func TestArchiveKickLogLocked_SkipsEmptyCapture(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "   \n\n  ")
	if m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archived an empty capture")
	}
	infos, _ := m.ListKickLogs("scanner")
	if len(infos) != 0 {
		t.Fatalf("archives = %d, want 0", len(infos))
	}
}

// Retention 0 is "archiving off"; no directory should even be created.
func TestArchiveKickLogLocked_RetentionZeroDisables(t *testing.T) {
	m, agent, dir := kickLogTestManager(t, "content")
	m.kickLogRetention = 0
	if m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archived with retention 0")
	}
	if _, err := os.Stat(filepath.Join(dir, "scanner")); !os.IsNotExist(err) {
		t.Errorf("agent archive dir exists, want none")
	}
}

// A capture failure is logged and swallowed — the kick/restart it decorates
// must proceed.
func TestArchiveKickLogLocked_CaptureErrorIsNonFatal(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "")
	m.terminal = funcTerminal{captureFullLog: func(*AgentProcess) (string, error) { return "", fmt.Errorf("boom") }}
	if m.archiveKickLogLocked(agent, "restart") {
		t.Fatal("archived despite capture error")
	}
}

func TestPruneKickLogs_KeepsNewestN(t *testing.T) {
	m, agent, dir := kickLogTestManager(t, "x")
	m.kickLogRetention = 3
	// Write 6 archives with strictly increasing timestamps.
	agentDir := filepath.Join(dir, "scanner")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("20260820-07000%d.000-kick%s", i, kickLogSuffix)
		if err := os.WriteFile(filepath.Join(agentDir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One more real archive triggers the prune. Its capture content makes 7.
	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archive failed")
	}
	infos, err := m.ListKickLogs("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("archives after prune = %d, want 3", len(infos))
	}
	// Newest-first: the just-written archive leads, then the two newest olds.
	if infos[0].Reason != "kick" || !strings.HasPrefix(infos[1].ID, "20260820-070005") || !strings.HasPrefix(infos[2].ID, "20260820-070004") {
		t.Errorf("unexpected survivors: %+v", infos)
	}
}

// The size cap prunes oldest-first but must never delete the newest archive,
// even when that archive alone exceeds the cap.
func TestPruneKickLogs_SizeCapKeepsNewest(t *testing.T) {
	m, agent, dir := kickLogTestManager(t, strings.Repeat("y", 4096))
	m.kickLogMaxBytes = 1024
	agentDir := filepath.Join(dir, "scanner")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(agentDir, "20260820-070000.000-kick"+kickLogSuffix)
	if err := os.WriteFile(old, []byte(strings.Repeat("z", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archive failed")
	}
	infos, _ := m.ListKickLogs("scanner")
	if len(infos) != 1 {
		t.Fatalf("archives = %d, want only the newest to survive the size cap", len(infos))
	}
	if !strings.HasPrefix(infos[0].ID, time.Now().UTC().Format("2006")) {
		t.Errorf("survivor is not the newest archive: %+v", infos)
	}
}

// The rotation invariant: on a kick with pending output, the capture and
// clear-history run BEFORE any input is sent to the pane. If the capture ran
// after the C-c/C-u clearing or after the kick text, the archived log would
// be polluted or the scrollback already mutated.
func TestDeliverKickLocked_ArchivesAndClearsBeforeInput(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "previous kick output")
	var events []string
	m.terminal = funcTerminal{
		captureFullLog: func(*AgentProcess) (string, error) {
			events = append(events, "capture")
			return "previous kick output", nil
		},
		clearHistory: func(*AgentProcess) { events = append(events, "clear") },
		sendKeys: func(_ *AgentProcess, keys ...string) {
			events = append(events, "sendkeys:"+strings.Join(keys, "+"))
		},
		captureVisiblePane: func(*AgentProcess) string { return "" },
	}
	agent.kickLogPending = true

	m.deliverKickLocked(agent, "next task", "send-kick")

	if len(events) < 3 || events[0] != "capture" || events[1] != "clear" {
		t.Fatalf("events = %v, want capture then clear before any pane input", events)
	}
	for _, e := range events[2:] {
		if e == "capture" || e == "clear" {
			t.Fatalf("capture/clear repeated after input started: %v", events)
		}
	}
	if !agent.kickLogPending {
		t.Error("kickLogPending = false after delivery, want true (new kick output is now pending)")
	}
	infos, _ := m.ListKickLogs("scanner")
	if len(infos) != 1 {
		t.Fatalf("archives = %d, want 1", len(infos))
	}
}

// The very first kick into a fresh session has no previous kick output;
// rotation must not archive the boot banner or clear anything.
func TestDeliverKickLocked_NoRotationWithoutPendingOutput(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "boot banner")
	captured := false
	m.terminal = funcTerminal{
		captureFullLog:     func(*AgentProcess) (string, error) { captured = true; return "boot banner", nil },
		sendKeys:           func(*AgentProcess, ...string) {},
		captureVisiblePane: func(*AgentProcess) string { return "" },
	}

	m.deliverKickLocked(agent, "first task", "startup")

	if captured {
		t.Error("capture ran with no pending kick output")
	}
	if infos, _ := m.ListKickLogs("scanner"); len(infos) != 0 {
		t.Errorf("archives = %d, want 0", len(infos))
	}
	if !agent.kickLogPending {
		t.Error("kickLogPending = false after first kick, want true")
	}
}

// Restart must snapshot the outgoing session's kick output before the session
// is destroyed — and the snapshot must survive even when the relaunch itself
// fails (no tmux in the environment): the archive is the whole point.
func TestRestart_ArchivesPendingKickOutput(t *testing.T) {
	m, agent, dir := kickLogTestManager(t, "output of the run being restarted")
	m.terminal = funcTerminal{
		captureFullLog: func(*AgentProcess) (string, error) { return "output of the run being restarted", nil },
		sendKeys:       func(*AgentProcess, ...string) {},
	}
	agent.kickLogPending = true
	// Paused short-circuits Restart right after ensureTmuxSession, keeping the
	// test away from token minting and a real CLI launch.
	agent.Paused = true
	m.workDir = t.TempDir()
	t.Cleanup(func() { _ = testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run() })

	_ = m.Restart(context.Background(), "scanner") // relaunch outcome irrelevant here

	if agent.kickLogPending {
		t.Error("kickLogPending still true after restart")
	}
	infos, err := m.ListKickLogs("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Reason != "restart" {
		t.Fatalf("archives = %+v, want one with reason restart", infos)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scanner", infos[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "output of the run being restarted") {
		t.Error("archive missing the pre-restart scrollback")
	}
}

// Graceful shutdown archives every agent with pending kick output, so a pod
// roll or hive upgrade cannot destroy the latest run's log (#4296).
func TestArchiveAllKickLogs(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "in-flight output")
	quiet := &AgentProcess{Name: "quiet", tmuxSession: "hive-quiet", Config: config.AgentConfig{}}
	m.agents["quiet"] = quiet
	agent.kickLogPending = true // scanner has un-archived output; quiet does not

	m.ArchiveAllKickLogs("shutdown")

	if agent.kickLogPending {
		t.Error("scanner still pending after shutdown archive")
	}
	infos, _ := m.ListKickLogs("scanner")
	if len(infos) != 1 || infos[0].Reason != "shutdown" {
		t.Fatalf("scanner archives = %+v, want one with reason shutdown", infos)
	}
	if quietInfos, _ := m.ListKickLogs("quiet"); len(quietInfos) != 0 {
		t.Errorf("quiet archives = %d, want 0 (nothing pending)", len(quietInfos))
	}
}

func TestListKickLogs_UnknownAgent(t *testing.T) {
	m, _, _ := kickLogTestManager(t, "x")
	if _, err := m.ListKickLogs("nope"); err == nil {
		t.Fatal("want error for unknown agent")
	}
}

// No archive directory yet is "no history", not an error — new code must not
// crash when less history is available than normal (#4296).
func TestListKickLogs_NoHistoryYet(t *testing.T) {
	m, _, _ := kickLogTestManager(t, "x")
	infos, err := m.ListKickLogs("scanner")
	if err != nil {
		t.Fatalf("ListKickLogs: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("archives = %d, want 0", len(infos))
	}
}

func TestReadKickLog_RejectsTraversalAndBadIDs(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "content")
	agent.kickLogPending = true
	m.rotateKickLogOnKickLocked(agent)

	for _, id := range []string{
		"",
		"../../etc/passwd",
		"../other/20260820-070000.000-kick.log",
		"sub/20260820-070000.000-kick.log",
		"20260820-070000.000-kick.txt",
		"a b.log",
		"..\\evil.log",
	} {
		if _, err := m.ReadKickLog("scanner", id); err == nil {
			t.Errorf("ReadKickLog accepted invalid id %q", id)
		}
	}
	if _, err := m.ReadKickLog("scanner", "20990101-000000.000-kick.log"); err == nil {
		t.Error("ReadKickLog found a nonexistent archive")
	}
}

func TestReadKickLog_RoundTrip(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "the run output")
	agent.kickLogPending = true
	m.rotateKickLogOnKickLocked(agent)

	infos, err := m.ListKickLogs("scanner")
	if err != nil || len(infos) != 1 {
		t.Fatalf("infos = %v, err = %v", infos, err)
	}
	body, err := m.ReadKickLog("scanner", infos[0].ID)
	if err != nil {
		t.Fatalf("ReadKickLog: %v", err)
	}
	if !strings.Contains(body, "the run output") || !strings.Contains(body, "==== hive kick log ====") {
		t.Errorf("unexpected archive body:\n%s", body)
	}
}

func TestValidKickLogID(t *testing.T) {
	valid := []string{"20260820-070000.000-kick.log", "a-b_c.log", "X1.log"}
	invalid := []string{"", ".log", "..", "a/b.log", "a\\b.log", "a..b.log", "a b.log", "a.txt", "é.log"}
	for _, id := range valid {
		if !validKickLogID(id) {
			t.Errorf("validKickLogID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if validKickLogID(id) {
			t.Errorf("validKickLogID(%q) = true, want false", id)
		}
	}
}

func TestSanitizeKickLogComponent(t *testing.T) {
	cases := map[string]string{
		"scanner":    "scanner",
		"weird/../x": "weird----x",
		"":           "agent",
		"a b":        "a-b",
	}
	for in, want := range cases {
		if got := sanitizeKickLogComponent(in); got != want {
			t.Errorf("sanitizeKickLogComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
