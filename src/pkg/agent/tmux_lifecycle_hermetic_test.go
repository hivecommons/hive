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
