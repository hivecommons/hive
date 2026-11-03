package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

// TestPollTmuxOutputForAgent_WiresThrashBreaker pins the poller-to-breaker
// WIRING end to end (#6297): a burst of policy-blocked lines rendered into a
// real tmux pane must leave the agent paused with trigger "thrash-breaker".
//
// The breaker logic itself (checkBlockedThrash, recordBlockedAndCheck) is
// already unit-tested in thrash_coverage_test.go / backend_coverage_test.go.
// What none of those pin is the single call site in pollTmuxOutputForAgent
// that makes the breaker fire in production — exactly the shape of #6147,
// where a correct breaker existed and nothing invoked it. If a poller refactor
// drops that call, this test fails; the unit tests would not.
//
// The assertion is on the pause (agent.Paused + PausedTrigger), not a log line.
func TestPollTmuxOutputForAgent_WiresThrashBreaker(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	// Keep server options and lifecycle independent of co-sharded tests.
	socket := fmt.Sprintf("hthrash%d", os.Getpid())
	tmux := func(args ...string) *exec.Cmd {
		return testTmuxCommandOnSocket(socket, args...)
	}
	session := "hive-thrashwire"
	// A WIDE session, not newRawTmuxSession: capture-pane returns wrapped
	// display lines (no -J), and at the default 80 columns the test shell's
	// long prompt wraps the injected line mid-marker ("git push bl / ocked:"),
	// which the breaker's Contains match never sees. 220 columns keeps each
	// injected marker on one captured line.
	if err := tmux("new-session", "-d", "-x", "220", "-y", "50", "-s", session, "env ENV= PS1= PS2= /bin/sh").Run(); err != nil {
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
	}
	t.Cleanup(func() {
		_ = tmux("kill-server").Run()
	})
	m := NewManager(map[string]config.AgentConfig{
		"thrashwire": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "thrashwire")
	m.mu.RLock()
	agent := m.agents["thrashwire"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.tmuxSocket = socket

	// Seed the pane BEFORE the poller starts: checkBlockedThrash only runs on
	// lines that are NEW relative to the poller's first capture (prevLines),
	// so the first tick must consume benign content and the blocked burst must
	// arrive strictly after it.
	if err := tmux("send-keys", "-t", session, "printf 'thrashwire pane seeded\\n'", "Enter").Run(); err != nil {
		t.Fatalf("seed pane: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pollerDone := make(chan struct{})
	go func() {
		m.pollTmuxOutputForAgent(agent, ctx)
		close(pollerDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-pollerDone:
		case <-time.After(10 * time.Second):
			t.Error("poller goroutine did not exit after cancel")
		}
	})

	// Wait for the poller's first capture (3s tick) so prevLines is set.
	firstCapture := time.Now().Add(15 * time.Second)
	for {
		agent.paneMu.Lock()
		captured := agent.lastPaneCapture != nil
		agent.paneMu.Unlock()
		if captured {
			break
		}
		if time.Now().After(firstCapture) {
			t.Fatal("poller never captured the seeded pane")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Drive a burst past the threshold (5 in 60s). One extra line absorbs a
	// tick landing mid-burst; unique suffixes keep every line a distinct diff
	// entry. All land well inside thrashWindow.
	// The lines must be real command OUTPUT starting with the helper's "⛔ "
	// glyph (#12015): a typed/echoed marker is displayed text, which the
	// breaker deliberately ignores. printf's octal escapes keep the typed
	// command ASCII while the rendered output carries the glyph.
	for i := 0; i < thrashThreshold+1; i++ {
		cmd := fmt.Sprintf(`printf '\342\233\224 git push blocked: advisory mode (attempt %d)\n'`, i)
		if err := tmux("send-keys", "-t", session, "-l", cmd).Run(); err != nil {
			t.Fatalf("send-keys: %v", err)
		}
		if err := tmux("send-keys", "-t", session, "Enter").Run(); err != nil {
			t.Fatalf("send Enter: %v", err)
		}
	}

	// The trip happens on the tick AFTER the burst renders, and Pause runs in
	// its own goroutine — poll for the paused state instead of sleeping.
	deadline := time.Now().Add(20 * time.Second)
	for {
		m.mu.RLock()
		paused := agent.Paused
		trigger := agent.PausedTrigger
		m.mu.RUnlock()
		if paused {
			if trigger != "thrash-breaker" {
				t.Fatalf("agent paused with trigger %q, want %q", trigger, "thrash-breaker")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocked-line burst through the poller never tripped the thrash breaker: pollTmuxOutputForAgent is not wired to checkBlockedThrash")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
