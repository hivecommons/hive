package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/watchdog"
)

// newWatchdogTestManager builds a Manager with one agent per backend, marked
// running, with the tmux seams faked so no subprocess ever runs.
func newWatchdogTestManager(t *testing.T, backends map[string]string) (*Manager, *map[string]string) {
	t.Helper()
	cfgs := make(map[string]config.AgentConfig, len(backends))
	for name, backend := range backends {
		cfgs[name] = config.AgentConfig{Backend: backend}
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	panes := make(map[string]string)
	m.visiblePaneCapture = func(a *AgentProcess) string { return panes[a.Name] }

	origExists := tmuxSessionExists
	tmuxSessionExists = func(_ *Manager, _ *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	for name := range backends {
		m.agents[name].State = StateRunning
		m.agents[name].HasLaunched = true
	}
	return m, &panes
}

// TestWatchdogObserveClassificationPerBackend runs REAL pane fixtures through
// the adapter (the manager's own cliPaneMarkers / paneShowsLoginPrompt
// tables) and the watchdog state machine, per backend — the RFC's
// getCLIState/classifyTmuxPane idiom end to end.
func TestWatchdogObserveClassificationPerBackend(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	settings := watchdog.DefaultSettings()
	staleChange := now.Add(-time.Hour)
	freshChange := now.Add(-time.Minute)

	cases := []struct {
		name       string
		backend    string
		pane       string
		lastChange time.Time
		needsLogin bool
		want       watchdog.PaneClass
	}{
		{
			name: "claude ready prompt", backend: "claude",
			pane: "Claude\n❯ ", lastChange: freshChange,
			want: watchdog.ClassReady,
		},
		{
			name: "claude login expired (RFC failure mode 1)", backend: "claude",
			pane: "● Login expired · Please run /login", lastChange: staleChange,
			want: watchdog.ClassAuthRequired,
		},
		{
			name: "claude oauth device screen", backend: "claude",
			pane: "Use the url below to sign in\nPaste code here if prompted", lastChange: staleChange,
			want: watchdog.ClassAuthRequired,
		},
		{
			name: "poller NeedsLogin flag alone", backend: "claude",
			pane: "Claude\n❯ ", lastChange: freshChange, needsLogin: true,
			want: watchdog.ClassAuthRequired,
		},
		{
			name: "agy first-run picker held for hours (RFC failure mode 2)", backend: "agy",
			pane: "Choose an accent\n[Next]  ↑/↓ Navigate", lastChange: staleChange,
			want: watchdog.ClassStuckOverlay,
		},
		{
			name: "codex ready banner", backend: "codex",
			pane: "OpenAI Codex\n› ", lastChange: freshChange,
			want: watchdog.ClassReady,
		},
		{
			name: "bob idle input box", backend: "bob",
			pane: "Bob-Shell\nEnter your prompt, / for commands", lastChange: freshChange,
			want: watchdog.ClassReady,
		},
		{
			name: "copilot device-flow login", backend: "copilot",
			pane: "Enter one-time code\ngithub.com/login/device", lastChange: staleChange,
			want: watchdog.ClassAuthRequired,
		},
		{
			name: "gemini dead at bash", backend: "gemini",
			pane: "agent@spoke:~$", lastChange: staleChange,
			want: watchdog.ClassShellPrompt,
		},
		{
			name: "pi context meter is ready", backend: "pi",
			pane: "↑37k ↓20k R756k CH99.6% $0.013 5.9%/1.0M (auto)", lastChange: freshChange,
			want: watchdog.ClassReady,
		},
		{
			name: "silent pane past grace", backend: "claude",
			pane: "", lastChange: staleChange,
			want: watchdog.ClassNoOutput,
		},
		{
			name: "unclassifiable output stays unknown", backend: "claude",
			pane: "streamed model tokens with no chrome at all", lastChange: staleChange,
			want: watchdog.ClassUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, panes := newWatchdogTestManager(t, map[string]string{"a1": tc.backend})
			(*panes)["a1"] = tc.pane
			m.agents["a1"].NeedsLogin = tc.needsLogin
			m.agents["a1"].LastPaneChange = tc.lastChange

			fleet := WatchdogFleet{M: m}
			obs, err := fleet.Observe("a1")
			if err != nil {
				t.Fatal(err)
			}
			if obs.Backend != tc.backend {
				t.Fatalf("backend = %q, want %q", obs.Backend, tc.backend)
			}
			got := watchdog.Classify(obs, now, settings)
			if got.Class != tc.want {
				t.Fatalf("classified %s (%s), want %s", got.Class, got.Reason, tc.want)
			}
		})
	}
}

func TestWatchdogObserveMissingSession(t *testing.T) {
	m, panes := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
	(*panes)["a1"] = "❯"
	tmuxSessionExists = func(_ *Manager, _ *AgentProcess) bool { return false }

	obs, err := WatchdogFleet{M: m}.Observe("a1")
	if err != nil {
		t.Fatal(err)
	}
	if obs.SessionExists {
		t.Fatal("session must be reported missing")
	}
	got := watchdog.Classify(obs, time.Now(), watchdog.DefaultSettings())
	if got.Class != watchdog.ClassNoSession {
		t.Fatalf("classified %s, want no-session", got.Class)
	}
	if obs.Pane != "" {
		t.Fatal("no capture must be attempted against a missing session")
	}
}

func TestWatchdogObserveUnknownAgent(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
	if _, err := (WatchdogFleet{M: m}).Observe("ghost"); err == nil {
		t.Fatal("unknown agent must error, not fabricate an observation")
	}
}

func TestWatchdogAgentNamesOnlyRunningLaunchedAgents(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{
		"running": "claude", "stopped": "claude", "unlaunched": "claude",
	})
	m.agents["stopped"].State = StateStopped
	m.agents["unlaunched"].HasLaunched = false

	names := WatchdogFleet{M: m}.AgentNames()
	if len(names) != 1 || names[0] != "running" {
		t.Fatalf("AgentNames = %v, want [running] — stopped/unlaunched agents belong to the crash-recovery loop", names)
	}
}

func TestWatchdogFleetPauseRestartDelegation(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
	fleet := WatchdogFleet{M: m}

	if fleet.IsPaused("a1") {
		t.Fatal("agent starts unpaused")
	}
	if err := fleet.Pause("a1", watchdog.CrashLoopTrigger, "test escalation"); err != nil {
		t.Fatal(err)
	}
	if !fleet.IsPaused("a1") {
		t.Fatal("pause must delegate to the manager")
	}
	if got := m.agents["a1"].PausedTrigger; got != watchdog.CrashLoopTrigger {
		t.Fatalf("PausedTrigger = %q, want %q", got, watchdog.CrashLoopTrigger)
	}
	if fleet.IsPaused("ghost") {
		t.Fatal("unknown agent is not paused")
	}
	if err := fleet.Restart(context.Background(), "ghost"); err == nil {
		t.Fatal("restarting an unknown agent must error")
	}
}

func TestWatchdogSetConditionsFlowsToSnapshot(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
	fleet := WatchdogFleet{M: m}
	conds := []watchdog.Condition{{
		Type: watchdog.ConditionReady, Status: watchdog.ConditionFalse,
		Reason: "shell-prompt", LastTransitionTime: time.Now(),
	}}
	fleet.SetConditions("a1", conds)
	fleet.SetConditions("ghost", conds) // must not panic

	snap := m.AllStatuses()["a1"]
	if len(snap.WatchdogConditions) != 1 || snap.WatchdogConditions[0].Reason != "shell-prompt" {
		t.Fatalf("conditions must ride AllStatuses snapshots, got %+v", snap.WatchdogConditions)
	}
}

func TestWatchdogLastProduction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude", "b1": "bob"})
	fleet := WatchdogFleet{M: m}

	// No evidence anywhere: pane never changed, no state dirs.
	if _, ok := fleet.LastProduction("a1"); ok {
		t.Fatal("no evidence must report ok=false")
	}
	if _, ok := fleet.LastProduction("ghost"); ok {
		t.Fatal("unknown agent has no evidence")
	}

	// Pane activity alone is evidence.
	paneTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	m.agents["a1"].LastPaneChange = paneTime
	got, ok := fleet.LastProduction("a1")
	if !ok || !got.Equal(paneTime) {
		t.Fatalf("LastProduction = %v ok=%v, want pane change %v", got, ok, paneTime)
	}

	// A newer conversation-file mtime wins.
	projDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	convo := filepath.Join(projDir, "session.jsonl")
	if err := os.WriteFile(convo, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(convo, newer, newer); err != nil {
		t.Fatal(err)
	}
	got, ok = fleet.LastProduction("a1")
	if !ok || !got.Equal(newer) {
		t.Fatalf("LastProduction = %v ok=%v, want state-file mtime %v", got, ok, newer)
	}

	// A backend with no known state dirs still reports pane evidence.
	m.agents["b1"].LastPaneChange = paneTime
	got, ok = fleet.LastProduction("b1")
	if !ok || !got.Equal(paneTime) {
		t.Fatalf("bob LastProduction = %v ok=%v, want %v", got, ok, paneTime)
	}
}

func TestNewestMtimeBounds(t *testing.T) {
	if _, ok := newestMtime(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("missing dir has no mtime")
	}
	root := t.TempDir()
	// Files below the depth bound are ignored.
	deep := filepath.Join(root, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	deepFile := filepath.Join(deep, "too-deep.txt")
	if err := os.WriteFile(deepFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(deepFile, future, future); err != nil {
		t.Fatal(err)
	}
	shallow := filepath.Join(root, "recent.txt")
	if err := os.WriteFile(shallow, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(shallow, want, want); err != nil {
		t.Fatal(err)
	}
	got, ok := newestMtime(root)
	if !ok || !got.Equal(want) {
		t.Fatalf("newestMtime = %v ok=%v, want shallow file %v (depth bound must hold)", got, ok, want)
	}
}
