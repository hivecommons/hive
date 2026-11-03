package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// writeFakeProc lays down /proc/<pid>/{stat,status} for one process. comm
// deliberately contains a space and a ')' so the ppid parse has to anchor
// on the last ')' rather than the first.
func writeFakeProc(t *testing.T, root string, pid, ppid, rssKB int) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (copilot (arm64) x) S %d %d %d 0 -1 4194560 0", pid, ppid, pid, pid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\tcopilot\nPid:\t%d\nPPid:\t%d\nVmPeak:\t%d kB\nVmRSS:\t%d kB\nThreads:\t12\n", pid, ppid, rssKB*2, rssKB)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProcTreeRSSMBSumsDescendantsOnly(t *testing.T) {
	root := t.TempDir()
	// pane shell 100 → copilot 200 → {helper 300, helper 301}; 400 is an
	// unrelated sibling under init and must not be counted. 500 has no
	// VmRSS line (kernel thread shape) and sits under the shell.
	writeFakeProc(t, root, 1, 0, 1024)
	writeFakeProc(t, root, 100, 1, 2*1024)
	writeFakeProc(t, root, 200, 100, 900*1024)
	writeFakeProc(t, root, 300, 200, 50*1024)
	writeFakeProc(t, root, 301, 200, 60*1024)
	writeFakeProc(t, root, 400, 1, 700*1024)
	if err := os.MkdirAll(filepath.Join(root, "500"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "500", "stat"), []byte("500 (kworker) S 100 0 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "500", "status"), []byte("Name:\tkworker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-pid entries (self, sys, …) are ignored.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got, want := procTreeRSSMB(root, 100), 2+900+50+60; got != want {
		t.Fatalf("procTreeRSSMB(100) = %d, want %d", got, want)
	}
	if got, want := procTreeRSSMB(root, 200), 900+50+60; got != want {
		t.Fatalf("procTreeRSSMB(200) = %d, want %d", got, want)
	}
	if got := procTreeRSSMB(root, 0); got != 0 {
		t.Fatalf("procTreeRSSMB(0) = %d, want 0", got)
	}
	if got := procTreeRSSMB(filepath.Join(root, "missing"), 100); got != 0 {
		t.Fatalf("procTreeRSSMB(missing root) = %d, want 0", got)
	}
	if got := procTreeRSSMB(root, 999); got != 0 {
		t.Fatalf("procTreeRSSMB(unknown pid) = %d, want 0", got)
	}
}

func TestProcPPIDAnchorsOnLastParen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stat")
	if err := os.WriteFile(p, []byte("42 (a) b (c) d) R 7 42 42 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ppid, ok := procPPID(p); !ok || ppid != 7 {
		t.Fatalf("procPPID = %d,%v want 7,true", ppid, ok)
	}
	if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := procPPID(p); ok {
		t.Fatal("procPPID accepted garbage")
	}
	if _, ok := procPPID(filepath.Join(dir, "nope")); ok {
		t.Fatal("procPPID accepted a missing file")
	}
}

const (
	recycleIdlePane = "  Copilot CLI\n\n❯ \n  / commands · ? help\n"
	recycleBusyPane = "  Copilot CLI\n\n◉ Working on it…  esc interrupt\n"
	recycleBarePane = "bash-5.2$ \n"
)

func TestSessionRecycleDecision(t *testing.T) {
	now := time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)
	policy := config.SessionRecycleConfig{MaxRSSMB: 1000, MaxAge: 6 * time.Hour, MinIdle: 2 * time.Minute}
	off := false
	base := func() sessionRecycleInput {
		return sessionRecycleInput{
			Policy:       policy,
			RSSMB:        500,
			StartedAt:    now.Add(-time.Hour),
			LastActivity: now.Add(-10 * time.Minute),
			Pane:         recycleIdlePane,
		}
	}
	cases := []struct {
		name   string
		mutate func(*sessionRecycleInput)
		want   bool
	}{
		{"under both budgets", func(in *sessionRecycleInput) {}, false},
		{"over rss and idle", func(in *sessionRecycleInput) { in.RSSMB = 1500 }, true},
		{"over rss but pane busy", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.Pane = recycleBusyPane }, false},
		{"over rss but no CLI in pane", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.Pane = recycleBarePane }, false},
		{"over rss but kick in flight", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.KickInFlight = true }, false},
		{"over rss but recent activity", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.LastActivity = now.Add(-30 * time.Second) }, false},
		{"over rss, no activity evidence, old session", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.LastActivity = time.Time{} }, true},
		{"over age and idle", func(in *sessionRecycleInput) { in.StartedAt = now.Add(-7 * time.Hour) }, true},
		{"over age but busy", func(in *sessionRecycleInput) { in.StartedAt = now.Add(-7 * time.Hour); in.Pane = recycleBusyPane }, false},
		{"over rss within backoff", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.LastRecycle = now.Add(-5 * time.Minute) }, false},
		{"over rss past backoff", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.LastRecycle = now.Add(-11 * time.Minute) }, true},
		{"over rss but within startup grace", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.StartedAt = now.Add(-time.Minute) }, false},
		{"over rss but unknown start", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.StartedAt = time.Time{} }, false},
		{"over rss but disabled", func(in *sessionRecycleInput) { in.RSSMB = 1500; in.Policy.Enabled = &off }, false},
		{"rss exactly at budget", func(in *sessionRecycleInput) { in.RSSMB = 1000 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			got := sessionRecycleDecision(in, now)
			if (got != "") != tc.want {
				t.Fatalf("decision = %q, want recycle=%v", got, tc.want)
			}
		})
	}
}

func TestSessionRecycleDecisionReasonNamesBudget(t *testing.T) {
	now := time.Now()
	in := sessionRecycleInput{
		Policy:       config.SessionRecycleConfig{MaxRSSMB: 1000},
		RSSMB:        1200,
		StartedAt:    now.Add(-time.Hour),
		LastActivity: now.Add(-time.Hour),
		Pane:         recycleIdlePane,
	}
	if got := sessionRecycleDecision(in, now); got != "rss 1200 MiB over 1000 MiB budget" {
		t.Fatalf("rss reason = %q", got)
	}
	in.RSSMB = 10
	in.StartedAt = now.Add(-7 * time.Hour)
	if got := sessionRecycleDecision(in, now); got != "session age 7h0m0s over 6h0m0s budget" {
		t.Fatalf("age reason = %q", got)
	}
}

func TestSessionRecycleConfigEffectiveDefaults(t *testing.T) {
	var nilCfg *config.SessionRecycleConfig
	e := nilCfg.Effective()
	if e.MaxRSSMB != config.DefaultSessionRecycleMaxRSSMB || e.MaxAge != config.DefaultSessionRecycleMaxAge || e.MinIdle != config.DefaultSessionRecycleMinIdle {
		t.Fatalf("defaults = %+v", e)
	}
	if !nilCfg.IsEnabled() {
		t.Fatal("nil policy should be enabled by default")
	}
	off := false
	if (&config.SessionRecycleConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("enabled: false should disable")
	}
	partial := (&config.SessionRecycleConfig{MaxRSSMB: 2048}).Effective()
	if partial.MaxRSSMB != 2048 || partial.MaxAge != config.DefaultSessionRecycleMaxAge {
		t.Fatalf("partial = %+v", partial)
	}
}

func TestSampleAndRecycleSessionsPublishesRSSAndRestarts(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 100, 1, 1024)
	writeFakeProc(t, root, 200, 100, 1700*1024)
	oldRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = oldRoot })

	m := testManager(0)
	started := time.Now().Add(-time.Hour)
	agent := &AgentProcess{
		Name:                   "scanner",
		State:                  StateRunning,
		StartedAt:              &started,
		LastTranscriptActivity: time.Now().Add(-10 * time.Minute),
		tmuxSession:            "hive-scanner",
	}
	m.agents["scanner"] = agent
	fakePanePID := 100
	oldPID, oldRestart := sessionRecyclePanePID, sessionRecycleRestart
	t.Cleanup(func() { sessionRecyclePanePID, sessionRecycleRestart = oldPID, oldRestart })
	sessionRecyclePanePID = func(*Manager, *AgentProcess) int { return fakePanePID }
	var restarted []string
	sessionRecycleRestart = func(_ *Manager, _ context.Context, name, reason, detail string) (bool, error) {
		restarted = append(restarted, name+"|"+reason+"|"+detail)
		return true, nil
	}

	m.sampleAndRecycleSessions(t.Context(), []sessionRecycleCandidate{{name: "scanner", pane: recycleIdlePane}})
	if agent.SessionRSSMB != 1701 {
		t.Fatalf("SessionRSSMB = %d, want 1701", agent.SessionRSSMB)
	}
	if len(restarted) != 1 || restarted[0] != "scanner|session recycle|rss 1701 MiB over 1536 MiB budget" {
		t.Fatalf("restarts = %v", restarted)
	}
	if agent.SessionRecycles != 1 || agent.lastSessionRecycle.IsZero() {
		t.Fatalf("recycle bookkeeping not recorded: %+v", agent)
	}

	// Immediately over budget again: the per-agent backoff holds.
	m.sampleAndRecycleSessions(t.Context(), []sessionRecycleCandidate{{name: "scanner", pane: recycleIdlePane}})
	if len(restarted) != 1 {
		t.Fatalf("backoff ignored: restarts = %v", restarted)
	}

	// Busy pane, past backoff: RSS is still published but no restart.
	agent.lastSessionRecycle = time.Now().Add(-config.SessionRecycleBackoff - time.Minute)
	agent.SessionRSSMB = 0
	m.sampleAndRecycleSessions(t.Context(), []sessionRecycleCandidate{{name: "scanner", pane: recycleBusyPane}})
	if agent.SessionRSSMB != 1701 || len(restarted) != 1 {
		t.Fatalf("busy pane: rss=%d restarts=%v", agent.SessionRSSMB, restarted)
	}

	// Pane pid unknown: the last good sample is kept, nothing restarts.
	fakePanePID = 0
	m.sampleAndRecycleSessions(t.Context(), []sessionRecycleCandidate{{name: "scanner", pane: recycleBusyPane}})
	if agent.SessionRSSMB != 1701 {
		t.Fatalf("stale sample overwritten: %d", agent.SessionRSSMB)
	}

	// Snapshot carries the fields to the dashboard.
	if p := agent.snapshot(); p.SessionRSSMB != 1701 || p.SessionRecycles != 1 {
		t.Fatalf("snapshot = rss %d recycles %d", p.SessionRSSMB, p.SessionRecycles)
	}
}
