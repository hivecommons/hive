package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// newWatchdogTestManager builds a Manager with one agent per backend, marked
// running, with the tmux seams faked so no subprocess ever runs.
func newWatchdogTestManager(t *testing.T, backends map[string]string) (*Manager, *map[string]string) {
	t.Helper()
	// NewManager silently loads whatever uid-map sits at UIDMapPath, and the
	// TestMain path is shared by the whole binary. A map leaked there by an
	// earlier test (any writer that skips stubUIDMapPath — the #5580 chain,
	// re-observed as #5631) hands these agents real UIDs, so AgentHome
	// resolves to a per-UID home instead of $HOME and LastProduction scans
	// the wrong directory, silently falling back to pane evidence. Watchdog
	// tests never exercise UID plumbing; isolate the path so no leak — past
	// or future — can reach them.
	stubUIDMapPath(t)
	cfgs := make(map[string]config.AgentConfig, len(backends))
	for name, backend := range backends {
		cfgs[name] = config.AgentConfig{Backend: backend}
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	panes := make(map[string]string)
	termSeams(m).captureVisiblePane = func(a *AgentProcess) string { return panes[a.Name] }

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

// TestWatchdogAgentNamesExcludesQuietByDesign asserts the quiet-by-design
// invariant: an agent whose pane is SUPPOSED to be silent is never handed to
// the reconciler, because the reconciler's only reading of a silent pane is
// "dead, restart it". On-demand agents idle until summoned and sandboxed
// agents hold no persistent tmux session at all, so both would be restarted
// forever. The plain running agent in the same table is the positive control:
// it proves the filter excludes these two classes specifically rather than
// returning an empty list for some unrelated reason.
func TestWatchdogAgentNamesExcludesQuietByDesign(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{
		"plain": "claude", "ondemand": "claude", "sandboxed": "claude",
	})

	onDemand := m.agents["ondemand"].Config
	onDemand.OnDemand = true
	m.agents["ondemand"].Config = onDemand

	// SandboxEnabled is an AND of the global switch and the per-agent
	// override, so both have to be set for the agent to actually be sandboxed.
	m.sandboxConfig = config.AgentSandboxConfig{Enabled: true}
	sandboxed := m.agents["sandboxed"].Config
	enabled := true
	sandboxed.Sandbox = &config.AgentSandboxOverride{Enabled: &enabled}
	m.agents["sandboxed"].Config = sandboxed

	// Positive control: the same manager, same states — only the two
	// quiet-by-design flags differ.
	if !m.agents["sandboxed"].Config.SandboxEnabled(m.sandboxConfig) {
		t.Fatal("precondition: the sandboxed agent must actually read as sandboxed")
	}

	names := WatchdogFleet{M: m}.AgentNames()
	if len(names) != 1 || names[0] != "plain" {
		t.Fatalf("AgentNames = %v, want [plain] — on-demand and sandboxed agents are quiet by design and must never be reconciled", names)
	}
}

// TestDeadSessionRecoveryOwnershipTransfer asserts the handover is real in
// both directions: with the watchdog owning recovery the manager's crash loop
// stops restarting missing-session agents, and with ownership back it restarts
// them again. The owned-by-crash-loop case is the positive control — without
// it, the first assertion could pass because the fixture never looked crashed.
func TestDeadSessionRecoveryOwnershipTransfer(t *testing.T) {
	newCrashedManager := func(t *testing.T) *Manager {
		t.Helper()
		m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
		// The session is gone: the crash loop's missing-session branch.
		orig := tmuxSessionExists
		tmuxSessionExists = func(_ *Manager, _ *AgentProcess) bool { return false }
		t.Cleanup(func() { tmuxSessionExists = orig })
		started := time.Now().Add(-time.Hour)
		m.agents["a1"].StartedAt = &started
		return m
	}

	t.Run("positive control: crash loop owns it by default", func(t *testing.T) {
		m := newCrashedManager(t)
		restarted := m.CheckAndRestartCrashedAgents(context.Background())
		if len(restarted) != 1 || restarted[0] != "a1" {
			t.Fatalf("restarted = %v, want [a1] — the crash loop owns dead sessions by default", restarted)
		}
	})

	t.Run("watchdog owning recovery stops the crash loop restarting it", func(t *testing.T) {
		m := newCrashedManager(t)
		m.SetDeadSessionRecoveryOwner(true)
		restarted := m.CheckAndRestartCrashedAgents(context.Background())
		if len(restarted) != 0 {
			t.Fatalf("restarted = %v, want none: two owners means the watchdog's ladder throttles nothing", restarted)
		}
	})

	t.Run("ownership is reversible", func(t *testing.T) {
		m := newCrashedManager(t)
		m.SetDeadSessionRecoveryOwner(true)
		if n := len(m.CheckAndRestartCrashedAgents(context.Background())); n != 0 {
			t.Fatalf("precondition: watchdog-owned must not restart, got %d", n)
		}
		m.SetDeadSessionRecoveryOwner(false)
		if n := len(m.CheckAndRestartCrashedAgents(context.Background())); n != 1 {
			t.Fatalf("handing ownership back must restore crash-loop recovery, got %d", n)
		}
	})
}

// TestWatchdogQueuedWork covers the readiness noise gate's inputs: an absent
// queue source is Unknown (never assumed empty), and an advisory-tier agent
// reports an empty queue because it cannot drain one by design.
func TestWatchdogQueuedWork(t *testing.T) {
	t.Run("no queue source is unknown, not empty", func(t *testing.T) {
		m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
		if _, known := (WatchdogFleet{M: m}).QueuedWork("a1"); known {
			t.Fatal("an absent queue source must report unknown, never a confident zero")
		}
	})

	t.Run("queue source is consulted", func(t *testing.T) {
		m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
		fleet := WatchdogFleet{M: m, Queued: func() (int, bool) { return 7, true }}
		n, known := fleet.QueuedWork("a1")
		if !known || n != 7 {
			t.Fatalf("QueuedWork = (%d, %v), want (7, true)", n, known)
		}
	})

	t.Run("advisory agents cannot drain the queue", func(t *testing.T) {
		m, _ := newWatchdogTestManager(t, map[string]string{"advisor": "claude"})
		cfg := m.agents["advisor"].Config
		cfg.Mode = "ADVISORY"
		m.agents["advisor"].Config = cfg

		fleet := WatchdogFleet{M: m, Queued: func() (int, bool) { return 42, true }}
		n, known := fleet.QueuedWork("advisor")
		if !known || n != 0 {
			t.Fatalf("QueuedWork = (%d, %v), want (0, true): a backlog an advisory agent cannot touch is not its fault", n, known)
		}
	})

	t.Run("unknown agent is unknown", func(t *testing.T) {
		m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
		fleet := WatchdogFleet{M: m, Queued: func() (int, bool) { return 7, true }}
		if _, known := fleet.QueuedWork("ghost"); known {
			t.Fatal("an unknown agent must not report a confident queue depth")
		}
	})
}

// TestWatchdogObserveCarriesStartedAt asserts the launch timestamp reaches the
// classifier, which is what lets boot grace suppress dead verdicts.
func TestWatchdogObserveCarriesStartedAt(t *testing.T) {
	m, _ := newWatchdogTestManager(t, map[string]string{"a1": "claude"})
	started := time.Date(2026, 8, 23, 11, 30, 0, 0, time.UTC)
	m.agents["a1"].StartedAt = &started

	obs, err := WatchdogFleet{M: m}.Observe("a1")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.StartedAt.Equal(started) {
		t.Fatalf("StartedAt = %v, want %v — without it boot grace cannot apply", obs.StartedAt, started)
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

	// Pin the invariant every assertion below rests on: with UID 0, AgentHome
	// is $HOME, so the evidence written under this test's HOME is the evidence
	// LastProduction scans. A nonzero UID means a uid-map leaked into
	// NewManager and re-routed the scan to a per-UID home — which used to
	// surface as a baffling "LastProduction is the pane time, two hours off"
	// failure (#5631) instead of naming the cause.
	for _, name := range []string{"a1", "b1"} {
		if uid := m.agents[name].UID; uid != 0 {
			t.Fatalf("agent %s inherited UID %d from a leaked uid-map; AgentHome would skip $HOME and miss the state-file evidence", name, uid)
		}
	}

	// No evidence anywhere: pane never changed, no state dirs.
	if _, ok := fleet.LastProduction("a1"); ok {
		t.Fatal("no evidence must report ok=false")
	}
	if _, ok := fleet.LastProduction("ghost"); ok {
		t.Fatal("unknown agent has no evidence")
	}

	// Both fixtures derive from ONE clock read so their 1h59m spread is fixed
	// by construction: pane evidence sits 2h back (coincidentally the
	// watchdog's NoProductionFor default — no production threshold is in play
	// here) and the state-file mtime 1m back, so the mtime must win.
	now := time.Now()

	// Pane activity alone is evidence.
	paneTime := now.Add(-2 * time.Hour).Truncate(time.Second)
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
	newer := now.Add(-time.Minute).Truncate(time.Second)
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

// TestWatchdogObserveCredentialProvenIsClaudeOnly pins the restriction that
// keeps the reconciler's alert suppression honest.
//
// Observation.CredentialProven exists so the watchdog can tell "login prompt
// over a credential a restart can redeem" from "genuinely logged out" and skip
// paging an operator for the former. That is only safe where the evidence is
// proof of USABILITY. credentialFileProves verifies an expiry for claude, but
// answers copilot and codex by the PRESENCE of a token file — and a
// stale-but-present copilot token is precisely the state an operator must be
// told about. Letting presence read as proof would silence the alert that is
// the only signal their fleet is logged out.
func TestWatchdogObserveCredentialProvenIsClaudeOnly(t *testing.T) {
	stageSharedClaudeCredential(t, map[string]any{
		"accessToken": "sk-ant-oat-live",
		"expiresAt":   time.Now().Add(4 * time.Hour).UnixMilli(),
	})
	m, panes := newWatchdogTestManager(t, map[string]string{
		"scanner": "claude",
		"helper":  "copilot",
	})
	// Presence-only evidence for copilot: credentialFileProves returns true on
	// a held token without ever checking whether it still works.
	m.SetCopilotToken("gho_stale_but_present")
	(*panes)["scanner"] = "❯ "
	(*panes)["helper"] = "❯ "

	fleet := WatchdogFleet{M: m}

	obs, err := fleet.Observe("scanner")
	if err != nil {
		t.Fatalf("observe claude agent: %v", err)
	}
	if !obs.CredentialProven {
		t.Fatal("claude agent with a live credential must report CredentialProven: its expiry is verifiable")
	}

	obs, err = fleet.Observe("helper")
	if err != nil {
		t.Fatalf("observe copilot agent: %v", err)
	}
	if obs.CredentialProven {
		t.Fatal("copilot evidence is presence-only and must never read as proof — doing so suppresses the operator's re-authentication alert")
	}
}
