package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ci_poll_wall_clock_test.go covers the second half of #9673's harness-guard
// ask: a single blocking `gh run watch` sits under ciPollNudgeThreshold (it
// is one command) but can still burn the whole kick, so nudgeIfPollingCI must
// also trigger once the kick has run past ciPollNudgeWallClock with at least
// one poll command seen. The count-threshold path (#9702) already had this
// coverage indirectly; these tests pin the new OR branch explicitly.

// ciPollManager builds a manager holding one CLI-backend agent whose pane and
// tmux calls are served from seams, mirroring nudgeManager in
// transient_api_error_nudge_test.go so no tmux server is involved.
func ciPollManager(t *testing.T, pane string, kickAge time.Duration) (*Manager, *AgentProcess) {
	t.Helper()
	m := testManager(4)
	kickedAt := time.Now().Add(-kickAge)
	a := &AgentProcess{
		Name:           "scanner",
		Config:         config.AgentConfig{Backend: "copilot"},
		tmuxSession:    "hive-scanner-test-nonexistent",
		LastKick:       &kickedAt,
		ciPollBaseline: -1,
	}
	m.agents[a.Name] = a
	termSeams(m).captureVisiblePane = func(*AgentProcess) string { return pane }
	termSeams(m).sessionAttached = func(*AgentProcess) bool { return false }
	termSeams(m).sendLiteral = func(*AgentProcess, string) {}
	return m, a
}

const ciPollIdlePane = "● Running `gh run watch 123`\n\n" + cliInputPromptMarker + " "

func TestNudgeIfPollingCIWallClock(t *testing.T) {
	tests := []struct {
		name        string
		kickAge     time.Duration
		polls       int // number of scrollback captures showing one new poll command each
		wantNudged  bool
		wantAwaited bool
	}{
		{
			name:        "one poll, kick young: neither trigger fires",
			kickAge:     2 * time.Minute,
			polls:       1,
			wantNudged:  false,
			wantAwaited: false,
		},
		{
			name:        "one poll, kick past wall clock: wall-clock trigger fires",
			kickAge:     11 * time.Minute,
			polls:       1,
			wantNudged:  true,
			wantAwaited: true,
		},
		{
			name:        "many polls, kick young: count trigger still fires",
			kickAge:     2 * time.Minute,
			polls:       ciPollNudgeThreshold + 1,
			wantNudged:  true,
			wantAwaited: true,
		},
		{
			name:        "zero new polls, kick past wall clock: no trigger",
			kickAge:     11 * time.Minute,
			polls:       0,
			wantNudged:  false,
			wantAwaited: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, a := ciPollManager(t, ciPollIdlePane, tt.kickAge)

			// Baseline capture establishes ciPollBaseline from an empty pane
			// so the poll commands below all read as "new this kick".
			m.nudgeIfPollingCI(a, "", ciPollIdlePane)

			scrollback := ""
			for i := 0; i < tt.polls; i++ {
				scrollback += "gh run watch 123\n"
			}
			m.nudgeIfPollingCI(a, scrollback, ciPollIdlePane)

			if a.ciPollNudgeSent != tt.wantNudged {
				t.Errorf("ciPollNudgeSent = %v, want %v", a.ciPollNudgeSent, tt.wantNudged)
			}
			if a.AwaitingCI != tt.wantAwaited {
				t.Errorf("AwaitingCI = %v, want %v", a.AwaitingCI, tt.wantAwaited)
			}
		})
	}
}

// TestNudgeIfPollingCIWallClockSecondNudgeSuppressed confirms the wall-clock
// path still honors the existing at-most-one-nudge-per-kick cap.
func TestNudgeIfPollingCIWallClockSecondNudgeSuppressed(t *testing.T) {
	m, a := ciPollManager(t, ciPollIdlePane, 11*time.Minute)
	m.nudgeIfPollingCI(a, "", ciPollIdlePane)
	m.nudgeIfPollingCI(a, "gh run watch 123\n", ciPollIdlePane)
	if !a.ciPollNudgeSent {
		t.Fatal("expected first wall-clock nudge to fire")
	}
	nudgesAfterFirst := a.CIPollNudges

	m.nudgeIfPollingCI(a, "gh run watch 123\ngh run watch 123\n", ciPollIdlePane)
	if a.CIPollNudges != nudgesAfterFirst {
		t.Errorf("CIPollNudges = %d, want unchanged at %d (already nudged this kick)", a.CIPollNudges, nudgesAfterFirst)
	}
}
