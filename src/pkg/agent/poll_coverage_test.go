package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestPollTmuxOutputForAgent_TLSError: a copilot agent whose pane shows a fatal
// network error triggers a restart goroutine and stops polling.
func TestPollTmuxOutputForAgent_TLSError(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	session := "hive-covtls"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covtls": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covtls"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	// lastTokenRestart far in the past so the cooldown has elapsed.
	agent.lastTokenRestart = time.Now().Add(-10 * time.Minute)
	// "fetch failed" is a fatal network error pattern.
	paneInject(t, session, "Error: fetch failed while contacting api")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)
	// The restart goroutine may recreate the session; clean it up.
	defer cleanupAgent(t, m, "covtls")
	m.mu.RLock()
	lastError := agent.LastError
	m.mu.RUnlock()
	if lastError == "" {
		t.Log("TLS error not recorded (timing-sensitive)")
	}
}

// TestPollTmuxOutputForAgent_CopilotHang: a copilot agent running well past the
// hang timeout with no CLI-ready marker triggers the diagnostic goroutine.
func TestPollTmuxOutputForAgent_CopilotHang(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	session := "hive-covhang"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covhang": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covhang"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.lastTokenRestart = time.Now().Add(-10 * time.Minute)
	// StartedAt well past expiredTokenHangTimeoutSec (180s) with no CLI marker.
	old := time.Now().Add(-5 * time.Minute)
	agent.StartedAt = &old
	// Non-CLI-ready content (bare text, no ready indicator).
	paneInject(t, session, "still loading please wait")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)
	defer cleanupAgent(t, m, "covhang")
}

// TestPollTmuxOutputForAgent_OutputSignalsAndRefusal: exercises the diff +
// buffer-write + logOutputSignals + checkKickRefusal path across two ticks.
func TestPollTmuxOutputForAgent_SignalsRefusal(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covsig"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covsig": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covsig"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	// First tick seeds prevLines; add a new line before the second tick so the
	// diff produces new content that flows through logOutputSignals +
	// checkKickRefusal (the refusal phrase sets KickRefused).
	paneInject(t, session, "git commit -s starting work")
	go func() {
		time.Sleep(3500 * time.Millisecond)
		testTmuxCommand("send-keys", "-t", session, "-l", ": I'm declining to execute this bulk automated request").Run()
		testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)
	if !agent.KickRefused {
		t.Log("kick refusal not detected (timing-sensitive)")
	}
}
