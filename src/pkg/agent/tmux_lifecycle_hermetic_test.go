package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func installHermeticTmux(t *testing.T, logCommands bool) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
if [ -n "${HIVE_FAKE_TMUX_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$HIVE_FAKE_TMUX_LOG"
fi
case "$*" in
  *has-session*) exit "${HIVE_FAKE_TMUX_HAS_SESSION_EXIT:-0}" ;;
  *capture-pane*) printf '%s' "$HIVE_FAKE_TMUX_OUTPUT"; exit "${HIVE_FAKE_TMUX_CAPTURE_EXIT:-0}" ;;
  *send-keys*)
    if [ -n "${HIVE_FAKE_TMUX_KEYS:-}" ]; then
      printf '%s\n' "$*" >> "$HIVE_FAKE_TMUX_KEYS"
    fi
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if logCommands {
		t.Setenv("HIVE_FAKE_TMUX_LOG", logPath)
	}
	return logPath
}

func TestHermeticTmuxSessionExistsAndCapturePane(t *testing.T) {
	logPath := installHermeticTmux(t, true)
	t.Setenv("HIVE_FAKE_TMUX_OUTPUT", "Copilot\n❯ ready\n")

	m := NewManager(nil, discardLogger(), ProjectContext{})
	if !m.tmuxSessionExists("hive-hermetic") {
		t.Fatal("fake has-session success should report session exists")
	}
	t.Setenv("HIVE_FAKE_TMUX_HAS_SESSION_EXIT", "1")
	if m.tmuxSessionExists("hive-hermetic") {
		t.Fatal("fake has-session failure should report session missing")
	}
	if got := m.captureTmuxPane("hive-hermetic"); !strings.Contains(got, "❯ ready") {
		t.Fatalf("captureTmuxPane = %q, want fake pane output", got)
	}
	if !m.tmuxPaneHasCLI("hive-hermetic") {
		t.Fatal("tmuxPaneHasCLI should detect CLI markers in fake capture")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "has-session -t hive-hermetic") ||
		!strings.Contains(logged, "capture-pane -t hive-hermetic -p -S") {
		t.Fatalf("fake tmux did not receive expected lifecycle commands:\n%s", logged)
	}
}

func TestHermeticWaitForLegacySessionReadyPaths(t *testing.T) {
	// This test only needs pane output. Avoid a command log in the TempDir:
	// package-level background tmux probes can inherit this fake through PATH
	// and race TempDir cleanup by recreating the log after RemoveAll starts.
	logPath := installHermeticTmux(t, false)
	t.Setenv("HIVE_FAKE_TMUX_OUTPUT", "goose is ready\n")

	m := NewManager(nil, discardLogger(), ProjectContext{})
	if !m.waitForCLIReady("hive-ready") {
		t.Fatal("waitForCLIReady should return true once the fake pane has a CLI marker")
	}
	if !m.waitForInputPrompt("hive-ready") {
		t.Fatal("waitForInputPrompt should return true once the fake pane has an input prompt")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("wait-path test created an unnecessary command log: %v", err)
	}
}

func TestHermeticWaitForInputPromptForAgentSkipsConsentScreen(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"worker": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["worker"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-worker"

	var mu sync.Mutex
	visibleCalls := 0
	termSeams(m).captureVisiblePane = func(*AgentProcess) string {
		mu.Lock()
		defer mu.Unlock()
		visibleCalls++
		if visibleCalls == 1 {
			return "Bypass Permissions mode\n❯ No, exit\nEnter to confirm\n"
		}
		return ""
	}
	termSeams(m).capturePane = func(*AgentProcess) string {
		return "goose is ready\n"
	}

	if !m.waitForInputPromptForAgent(agent) {
		t.Fatal("waitForInputPromptForAgent should return true after consent screen clears")
	}
	mu.Lock()
	defer mu.Unlock()
	if visibleCalls < 2 {
		t.Fatalf("visible pane checked %d times, want consent skip then ready poll", visibleCalls)
	}
}

func TestHermeticWatchForTrustPromptForAgentSendsBackendSpecificKeys(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"coder": {Backend: "codex"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["coder"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-coder"
	termSeams(m).capturePane = func(*AgentProcess) string {
		return "✨ Update available! 1.0.0 -> 1.0.1\n1. Update now\n3. Skip until next version\n"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var keys []string
	termSeams(m).sendKeys = func(_ *AgentProcess, sent ...string) {
		keys = append(keys, sent...)
		if len(sent) == 1 && sent[0] == "Enter" {
			cancel()
			close(done)
		}
	}

	go m.watchForTrustPromptForAgent(agent, ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not answer the fake codex update prompt")
	}
	if got, want := strings.Join(keys, ","), "3,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q", got, want)
	}
}

func TestPaneShowsBlockingPromptTailAndBackendMatching(t *testing.T) {
	oldPrompt := "✨ Update available! 1.0.0 -> 1.0.1\n3. Skip until next version\n" +
		strings.Repeat("old line\n", blockingPromptTailLines+1)
	if PaneShowsBlockingPrompt("codex", oldPrompt) {
		t.Fatal("old scrollback prompt outside the eligible tail should not block")
	}
	livePrompt := "recent output\n✨ Update available! 1.0.0 -> 1.0.1\n3. Skip until next version\n"
	if !PaneShowsBlockingPrompt("codex", livePrompt) {
		t.Fatal("live codex update prompt should be detected")
	}
	if PaneShowsBlockingPrompt("claude", livePrompt) {
		t.Fatal("codex prompt must not match a different backend")
	}
	if !PaneShowsBlockingPrompt("copilot", "Confirm folder trust\n1. Yes\n") {
		t.Fatal("copilot folder trust prompt should be detected")
	}
}
