package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

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
	// The seam runs on the watcher goroutine. It used to close(done) on every
	// "Enter", so a second answer to the still-displayed prompt panicked the
	// whole shard with "close of closed channel" (#6282, #6283): under -race
	// on a loaded runner the first answer can stall past trustReanswerAfter
	// with a tick already queued, and the watcher's select does not prefer
	// the cancelled ctx over that tick. The close is now owned by a
	// sync.Once, keys is guarded by a mutex (it is read from the test
	// goroutine after the watcher may still be running), and the test waits
	// for the watcher to exit before asserting so a late second answer is
	// reported as a failed assertion instead of a process-wide panic.
	var (
		mu       sync.Mutex
		keys     []string
		doneOnce sync.Once
	)
	done := make(chan struct{})
	termSeams(m).sendKeys = func(_ *AgentProcess, sent ...string) {
		mu.Lock()
		keys = append(keys, sent...)
		mu.Unlock()
		if len(sent) == 1 && sent[0] == "Enter" {
			cancel()
			doneOnce.Do(func() { close(done) })
		}
	}

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		m.watchForTrustPromptForAgent(agent, ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not answer the fake codex update prompt")
	}
	// The watcher must stop on the cancelled ctx without typing anything
	// else: a queued tick may not outrank cancellation (manager.go re-checks
	// ctx.Err() on the tick path). Waiting here also keeps the goroutine
	// from outliving the test and touching the seams of a later test.
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after its context was cancelled")
	}
	mu.Lock()
	got := strings.Join(keys, ",")
	mu.Unlock()
	if want := "3,Enter"; got != want {
		t.Fatalf("sent keys = %q, want %q (a second answer after cancel means the watcher ignored ctx)", got, want)
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
