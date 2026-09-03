package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// tmuxAvailable reports whether a usable tmux binary is on PATH. All tmux-based
// coverage tests skip when it is absent so the suite still passes in minimal CI.
//
// This gate stays PERMISSIVE under HIVE_TEST_REQUIRE_BEHAVIOURAL (#5388): a
// runner without tmux is a genuine capability difference, not a broken test,
// and making it fatal would manufacture false reds on any laptop or minimal
// image. The lane that wants these tests to actually run installs tmux and
// asserts that via the flag; everything DOWNSTREAM of this check — creating a
// session, driving a pane — is then a broken test if it fails, and those sites
// use testutil.SkipUnlessRequired. This mirrors the shell half, which likewise
// left docker- and tmux-gated skips permissive.
func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// forceSharedUID pins a test agent to the shared dev UID and the default tmux
// socket, making the test hermetic on live hive hosts.
//
// NewManager loads /var/run/hive/uid-map.json whenever it exists. On a host
// that runs (or ever ran) a real hive, a test agent whose name collides with a
// deployed agent — "scanner" is one — silently inherits that agent's real UID
// and per-UID tmux socket from the production map. Every tmux exec for it then
// routes through `su-exec <user>` (absent outside the hive container image) and
// targets a per-UID socket no test session lives on. CI runners have no
// uid-map, so the collision only bites on hive-like hosts: the suite is green
// in CI and red for anyone running it where a hive is installed.
func forceSharedUID(t *testing.T, m *Manager, name string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[name]
	if !ok {
		t.Fatalf("forceSharedUID: agent %q not found", name)
	}
	a.UID = 0
	a.tmuxSocket = ""
}

// cleanupAgent cancels the agent's poll goroutine and tears down its tmux
// session so a launched agent leaves nothing running after the test.
//
// It then blocks until the launch goroutines (pollTmuxOutputForAgent,
// watchForTrustPromptForAgent, deliverStartupKick, dismissInferencePrompts)
// have actually observed the cancellation and returned. Those goroutines read
// process env (via exec.Command / exec.LookPath) on their poll ticks, so if
// they leaked past the test they would race a subsequent t.Setenv("PATH", ...)
// env restore. Waiting past one poll interval (3s) drains them deterministically
// before the test's env-restore cleanup runs (defer runs before t.Cleanup).
func cleanupAgent(t *testing.T, m *Manager, name string) {
	t.Helper()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if ok && agent.cancel != nil {
		agent.cancel()
	}
	m.mu.Unlock()
	if ok {
		_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
	}
	// Drain launch goroutines before the test finishes. The startup-kick
	// goroutine (deliverStartupKick) is not ctx-aware and, once the session is
	// gone, polls waitForCLIReadyForAgent every 2s until its deadline — each tick
	// exec's tmux (an env read) which would race TestMain's PATH restore at
	// binary teardown. Launch tests use readyStub so the ❯ marker lets that
	// goroutine deliver and return within a couple of ticks; wait past that.
	time.Sleep(5 * time.Second)
}

// overrideStub rewrites an existing stub binary in the global stubBinDir (which
// TestMain already placed on PATH) so it prints firstLine then reads stdin,
// keeping the marker visible in the pane. It restores the original stub on
// cleanup. This does NOT mutate PATH — avoiding a data race between a leaked
// launch goroutine's env read and a t.Setenv env restore.
func overrideStub(t *testing.T, name, firstLine string) {
	t.Helper()
	p := filepath.Join(stubBinDir, name)
	orig, err := os.ReadFile(p)
	if err != nil {
		// TestMain writes every stub in this list unconditionally and
		// os.Exit(1)s if any write fails, so a missing stub here cannot mean
		// "unsuitable environment" — it means the harness changed underneath
		// this helper (a renamed stub, a mutated stubBinDir). Under the flag
		// that is a broken test, not a reason to stop asserting (#5388).
		testutil.SkipfUnlessRequired(t, "global stub %q not present: %v", name, err)
	}
	// Print the custom line AND the ❯ ready marker so readiness gates resolve
	// (so no exec-looping goroutine leaks past the test), while the custom line
	// still drives content-specific logic (auth-error/ready detection).
	script := "#!/bin/sh\nprintf '%s\\n\\342\\235\\257 ready\\n' '" + firstLine + "'\nexec cat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(p, orig, 0o755) })
}

// inferenceReadyStub makes the claude stub (which inference backends launch)
// render the "bypass permissions on" ready marker so the NON-ctx-aware
// dismissInferencePrompts goroutine returns on its first poll instead of
// spinning for its full 60s timeout — which would otherwise leave an
// exec-looping goroutine alive past the test, racing TestMain's env teardown.
func inferenceReadyStub(t *testing.T) {
	t.Helper()
	overrideStub(t, "claude", "bypass permissions on")
}

// newRawTmuxSession creates a bare tmux session on the package test tmux server
// and registers cleanup. Used to drive pane-inspection helpers without spawning
// a real CLI.
func newRawTmuxSession(t *testing.T, session string) {
	t.Helper()
	if err := testTmuxCommand("new-session", "-d", "-s", session).Run(); err != nil {
		// Every caller gates on tmuxAvailable() first, so tmux IS on PATH by
		// the time we get here and TMUX_TMPDIR points at a directory TestMain
		// created. A failure now is a broken test — a stale socket, a server
		// the suite failed to clean up, a session name collision — not a
		// missing capability (#5388).
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
	}
	t.Cleanup(func() {
		_ = testTmuxCommand("kill-session", "-t", session).Run()
	})
}

// paneInject renders literal text into a tmux pane so capture-pane sees it.
// It types the text as a literal shell comment line (prefixed with ": ")
// followed by Enter — the text appears verbatim in the pane without executing.
func paneInject(t *testing.T, session, text string) {
	t.Helper()
	// Send as a literal string; ": " makes bash treat the rest as a no-op arg
	// so nothing executes but the text is echoed into the visible pane.
	if err := testTmuxCommand("send-keys", "-t", session, "-l", ": "+text).Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	_ = testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	time.Sleep(400 * time.Millisecond)
}

// requirePaneShows blocks until capture-pane actually returns text in the
// session's visible pane, failing the test if it never renders.
//
// paneInject only types the text and sleeps a fixed 400ms; on a loaded runner
// (parallel packages, coverage instrumentation, the suite's own tmux fork
// storm) the server can take longer than that to paint. Tests that next enter
// a readiness poll bounded by the TestMain-shrunk cliReadyTimeout (5s) then
// start that clock BEFORE their marker is visible and flake with "kick
// dropped" (seen intermittently in TestDeliverStartupKick_BobReceivesKick).
// Gating on the render keeps those tests deterministic without widening the
// production timeouts they exist to exercise.
func requirePaneShows(t *testing.T, session, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := testTmuxCommand("capture-pane", "-t", session, "-p").Output()
		if err == nil && strings.Contains(string(out), text) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %q never rendered %q (capture err: %v, last capture:\n%s)",
				session, text, err, string(out))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Start / launchInTmux — full launch path with real tmux + stub binary
// ---------------------------------------------------------------------------

func TestStart_LaunchClaude(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "opus"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	forceSharedUID(t, m, "scanner")

	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "scanner")

	ap, _ := m.GetStatus("scanner")
	if ap.State != StateRunning {
		t.Errorf("expected running, got %s", ap.State)
	}
	if !ap.HasLaunched {
		t.Error("HasLaunched should be true")
	}
}

func TestStart_LaunchCopilotIssuesOnly(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot", Model: "auto", Mode: "ISSUES_ONLY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

func TestStart_LaunchGooseAdvisory(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "goose", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 1})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

func TestStart_LaunchInferenceBackend(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	inferenceReadyStub(t)
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "litellm", Model: "deepseek-r1"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	var routed bool
	m.SetInferenceCallbacks(func(string, string, string) { routed = true }, func(string) {})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	if !routed {
		t.Error("inference launch should fire route callback")
	}
}

func TestStart_UnknownBackendFailsGracefully(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "nonexistent-backend"},
	}, discardLogger(), ProjectContext{})
	// launchInTmux returns nil but marks the agent failed.
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start should not error on unknown backend: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	ap, _ := m.GetStatus("cxa")
	if ap.State != StateFailed {
		t.Errorf("unknown backend should mark agent failed, got %s", ap.State)
	}
}

func TestStart_PersistedOperatorPauseStaysPaused(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		// Config.Paused=true is a persisted operator pause; even an inference
		// backend must stay paused on Start.
		"cxa": {Backend: "litellm", Paused: true},
	}, discardLogger(), ProjectContext{})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	ap, _ := m.GetStatus("cxa")
	if ap.State != StatePaused {
		t.Errorf("persisted operator pause should stay paused, got %s", ap.State)
	}
}

func TestStart_TransientInferencePauseAutoUnpauses(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	inferenceReadyStub(t)
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "litellm"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["cxa"].Paused = true // transient (not Config.Paused, not dashboard-api)
	m.mu.Unlock()
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	ap, _ := m.GetStatus("cxa")
	if ap.State != StateRunning {
		t.Errorf("transient inference pause should auto-unpause, got %s", ap.State)
	}
}

// ---------------------------------------------------------------------------
// Restart / RestartWithBootstrap / RestartThenSendKick
// ---------------------------------------------------------------------------

func TestRestart_RunningAgent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(t, m, "cxa")
	if err := m.Restart(context.Background(), "cxa"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	ap, _ := m.GetStatus("cxa")
	if ap.RestartCount != 1 {
		t.Errorf("restart count = %d, want 1", ap.RestartCount)
	}
}

func TestRestart_PausedPreserved(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["cxa"].Paused = true
	m.mu.Unlock()
	if err := m.Restart(context.Background(), "cxa"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	ap, _ := m.GetStatus("cxa")
	if ap.State != StatePaused {
		t.Errorf("paused agent restart should stay paused, got %s", ap.State)
	}
}

func TestRestartWithBootstrap(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	if err := m.RestartWithBootstrap(context.Background(), "cxa", "custom bootstrap prompt"); err != nil {
		t.Fatalf("RestartWithBootstrap: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	ap, _ := m.GetStatus("cxa")
	if ap.State != StateRunning {
		t.Errorf("expected running, got %s", ap.State)
	}
}

func TestRestartWithBootstrap_NotFound(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	if err := m.RestartWithBootstrap(context.Background(), "ghost", "p"); err == nil {
		t.Error("expected error")
	}
}

func TestRestartThenSendKick_NotFound(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	if err := m.RestartThenSendKick(context.Background(), "ghost", "hi"); err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// SendKick error branches
// ---------------------------------------------------------------------------

func TestSendKick_NoTmuxSession(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["cxa"].State = StateRunning // running but no tmux session exists
	m.mu.Unlock()
	if err := m.SendKick("cxa", "hi"); err == nil {
		t.Error("expected tmux-session-not-found error")
	}
}

// ---------------------------------------------------------------------------
// deliverKickLocked — direct, against a real tmux session with a ready prompt
// ---------------------------------------------------------------------------

func TestDeliverKickLocked(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covkick"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{
		"covkick": {Backend: "claude", ClearOnKick: true},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["covkick"]
	agent.tmuxSession = session
	m.deliverKickLocked(agent, "do the thing", "test")
	m.mu.Unlock()

	if agent.LastKick == nil {
		t.Error("LastKick should be set")
	}
	if agent.LastKickMessage != "do the thing" {
		t.Errorf("LastKickMessage = %q", agent.LastKickMessage)
	}
	if len(agent.KickHistory) == 0 {
		t.Error("KickHistory should have an entry")
	}
}

func TestDeliverKickLocked_LongMessageChunks(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covchunk"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covchunk": {Backend: "goose"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["covchunk"]
	agent.tmuxSession = session
	// > chunkSize (400) runes to exercise chunk loop; goose skips the C-c clear.
	long := ""
	for i := 0; i < 500; i++ {
		long += "x"
	}
	m.deliverKickLocked(agent, long, "test")
	m.mu.Unlock()
	if agent.LastKickMessage != long {
		t.Error("long message should be recorded verbatim")
	}
}

func TestDeliverKickLocked_InferenceSuffix(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covinf"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covinf": {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["covinf"]
	agent.tmuxSession = session
	m.deliverKickLocked(agent, "hello", "test")
	m.mu.Unlock()
	// Inference backends get an action-forcing suffix appended.
	if agent.LastKickMessage == "hello" {
		t.Error("inference kick should append action suffix")
	}
}

// ---------------------------------------------------------------------------
// Pane inspection helpers against a real pane
// ---------------------------------------------------------------------------

func TestCaptureVisiblePane_And_WaitForInputPrompt(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covpane"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covpane": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["covpane"]
	agent.tmuxSession = session
	m.mu.Unlock()

	// Render a ready-prompt marker.
	paneInject(t, session, "goose is ready")

	if !m.waitForInputPromptForAgent(agent) {
		t.Error("waitForInputPromptForAgent should return true when prompt visible")
	}
	if !m.waitForCLIReadyForAgent(agent) {
		// pane contains "goose is ready" but marker check needs a cliPaneMarker;
		// "goose" is a marker, so this should also be true.
		t.Error("waitForCLIReadyForAgent should return true")
	}
	if pane := m.captureVisiblePaneForAgent(agent); pane == "" {
		t.Error("captureVisiblePaneForAgent returned empty")
	}
	if !m.tmuxPaneHasCLIForAgent(agent) {
		t.Error("tmuxPaneHasCLIForAgent should detect the goose marker")
	}
}

func TestWaitForInputPrompt_Session(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covwait"
	newRawTmuxSession(t, session)
	m := NewManager(nil, discardLogger(), ProjectContext{})
	// "goose is ready" satisfies the input-prompt check AND contains the
	// "goose" cliPaneMarker so the CLI-ready / has-CLI checks also pass.
	paneInject(t, session, "goose is ready")
	if !m.waitForInputPrompt(session) {
		t.Error("waitForInputPrompt should return true")
	}
	if !m.waitForCLIReady(session) {
		t.Error("waitForCLIReady should return true")
	}
	if !m.tmuxPaneHasCLI(session) {
		t.Error("tmuxPaneHasCLI should be true")
	}
	if m.captureTmuxPane(session) == "" {
		t.Error("captureTmuxPane returned empty")
	}
}

func TestTmuxSessionExists(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covexists"
	newRawTmuxSession(t, session)
	m := NewManager(nil, discardLogger(), ProjectContext{})
	if !m.tmuxSessionExists(session) {
		t.Error("session should exist")
	}
	if m.tmuxSessionExists("hive-does-not-exist-xyz") {
		t.Error("nonexistent session should not exist")
	}
}

// ---------------------------------------------------------------------------
// confirmMenuOption — drives a menu rendered in a real pane
// ---------------------------------------------------------------------------

func TestConfirmMenuOption_AlreadyDismissed(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covmenu"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covmenu": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covmenu"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	// Pane does NOT contain the title -> treated as already dismissed -> true.
	if !m.confirmMenuOption(agent, "Nonexistent Title", "Yes", "Down") {
		t.Error("missing title should be treated as dismissed (true)")
	}
}

func TestConfirmMenuOption_SelectsOption(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covmenu2"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covmenu2": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covmenu2"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	// Render a menu: the title line, and a selected "❯" line whose text
	// contains the wanted option. We print it with printf so the ❯ line is at column 0.
	if err := testTmuxCommand("send-keys", "-t", session, "-l", "printf 'Bypass Permissions mode\\n❯ 2. Yes I accept\\n'").Run(); err != nil {
		t.Fatal(err)
	}
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	// Wait until the shell has actually started and executed the printf: a
	// fixed 500ms was a knife-edge against shell startup time (a zsh with user
	// dotfiles takes >2s to first render on some hosts), and confirmMenuOption
	// treats a pane without the title as "already dismissed" — masking the
	// race as a wrong-answer failure four Down-keys later.
	rendered := false
	for i := 0; i < 100; i++ {
		if selectedMenuOption(m.captureVisiblePaneForAgent(agent)) != "" {
			rendered = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !rendered {
		t.Fatalf("menu never rendered in the pane: %q", m.captureVisiblePaneForAgent(agent))
	}
	// want "accept" is on the selected ❯ line -> confirm immediately.
	if !m.confirmMenuOption(agent, "Bypass Permissions mode", "accept", "Down") {
		t.Error("should confirm the already-selected wanted option")
	}
}

// ---------------------------------------------------------------------------
// dismissInferencePrompts — early return when main prompt visible
// ---------------------------------------------------------------------------

func TestDismissInferencePrompts_ReadyReturnsFast(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "covdismiss", "bypass permissions on", 0)
	runDismissPromptScript(t, m, agent, script, nil)
}

// ---------------------------------------------------------------------------
// pollTmuxOutputForAgent — one tick with real pane, cancellable
// ---------------------------------------------------------------------------

func TestPollTmuxOutputForAgent_OneTick(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covpoll"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covpoll": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covpoll"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	paneInject(t, session, "[HEARTBEAT] alive")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx) // returns when ctx expires
}

func TestWatchForTrustPromptForAgent_Cancel(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covtrust"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covtrust": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covtrust"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	paneInject(t, session, "Confirm folder trust")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.watchForTrustPromptForAgent(agent, ctx)
}

func TestWatchForTrustPrompt_Session(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covtrust2"
	newRawTmuxSession(t, session)
	m := NewManager(nil, discardLogger(), ProjectContext{})
	paneInject(t, session, "Do you trust the files")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.watchForTrustPrompt(session, ctx)
}

func TestPollTmuxOutput_Session(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covpoll2"
	newRawTmuxSession(t, session)
	m := NewManager(nil, discardLogger(), ProjectContext{})
	paneInject(t, session, "git commit -s")
	buf := NewRingBuffer(50)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	m.pollTmuxOutput("covpoll2", session, buf, ctx)
}

// ---------------------------------------------------------------------------
// Stop / RemoveAgent / AddAgent
// ---------------------------------------------------------------------------

func TestStop_RunningAndNotRunning(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	// Not running -> no-op nil.
	if err := m.Stop("cxa"); err != nil {
		t.Fatalf("Stop not-running: %v", err)
	}
	if err := m.Stop("ghost"); err == nil {
		t.Error("Stop unknown should error")
	}
	m.mu.Lock()
	m.agents["cxa"].State = StateRunning
	_, c := context.WithCancel(context.Background())
	m.agents["cxa"].cancel = c
	m.mu.Unlock()
	if err := m.Stop("cxa"); err != nil {
		t.Fatalf("Stop running: %v", err)
	}
}
