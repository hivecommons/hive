package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

func TestKickRecoveryHonorsRestartStormBackoff(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			fake := installAttachFakeTmux(t)
			m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
			agent := m.agents["scanner"]
			agent.State = StateRunning
			agent.restartStormBackoffUntil = time.Now().Add(time.Minute)
			termSeams(m).captureVisiblePane = func(*AgentProcess) string { return "dev@hive:~$ " }
			termSeams(m).sendLiteral = func(*AgentProcess, string) { t.Error("kick typed into a bare shell") }

			var err error
			if async {
				// Run the slow half inline so no delivery goroutine outlives the test.
				err = m.deliverKickAsync("scanner", "continue scanning")
			} else {
				err = m.SendKick("scanner", "continue scanning")
			}
			if err == nil || !strings.Contains(err.Error(), "restart-storm breaker; retry in") {
				t.Fatalf("kick error = %v, want restart backoff", err)
			}
			if agent.RestartCount != 0 || agent.kickEpoch != 0 {
				t.Fatal("backed-off kick must not restart or invalidate the session")
			}
			// The fake tmux creates its log lazily; a missing log means the
			// breaker short-circuited before any tmux call, which is fine.
			log, readErr := os.ReadFile(fake.logPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if strings.Contains(string(log), "kill-session") {
				t.Fatalf("backed-off kick tore down the session: %s", log)
			}
		})
	}
}

func TestKickRecoveryPausesAtRestartStormCap(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	agent := m.agents["scanner"]
	agent.State = StateRunning
	termSeams(m) // Pause's Ctrl+C stays inside the fake terminal.
	events := make([]RestartEvent, restartStormPauseAfter)
	for i := range events {
		events[i] = RestartEvent{At: time.Now().Add(-time.Duration(i) * time.Minute), Reason: "kick CLI recovery"}
	}
	m.SeedRestartTelemetry("scanner", len(events), events, "kick CLI recovery")

	err := m.restartForKick(context.Background(), "scanner")
	if err == nil || !strings.Contains(err.Error(), "restart-storm breaker") {
		t.Fatalf("recovery error = %v, want storm refusal", err)
	}
	if !agent.Paused || agent.PausedTrigger != RestartStormTrigger || agent.State != StatePaused {
		t.Fatalf("storm must pause scanner: state=%s trigger=%s", agent.State, agent.PausedTrigger)
	}
	if agent.RestartCount != len(events) || agent.kickEpoch != 0 {
		t.Fatal("storm cap must not perform another restart")
	}
}
