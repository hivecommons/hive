package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
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

// TestNudgeIfPollingCISkipsMonitoringLanes pins #13860, #13898 and #14188:
// reviewers, CI maintainers and quality agents read check status as evidence
// for the PRs and repositories they monitor, so that work must not be mistaken
// for polling their own PR.
func TestNudgeIfPollingCISkipsMonitoringLanes(t *testing.T) {
	for _, tt := range []struct {
		name       string
		agentName  string
		role       string
		wantNudged bool
	}{
		{name: "reviewer role", agentName: "scanner", role: ReviewerRole, wantNudged: false},
		{name: "reviewer name only", agentName: "code-reviewer-sonnet-1", role: "", wantNudged: false},
		{name: "CI maintainer role", agentName: "worker", role: "ci-maintainer", wantNudged: false},
		{name: "CI maintainer name only", agentName: "ci-maintainer", role: "", wantNudged: false},
		{name: "quality role", agentName: "worker", role: "quality", wantNudged: false},
		{name: "quality name only", agentName: "quality", role: "", wantNudged: false},
		{name: "developer", agentName: "scanner", role: "developer", wantNudged: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, a := ciPollManager(t, ciPollIdlePane, 2*time.Minute)
			tr := recordConditions(t, m)
			a.Name = tt.agentName
			a.Config.Role = tt.role
			sent := 0
			termSeams(m).sendLiteral = func(*AgentProcess, string) { sent++ }

			m.nudgeIfPollingCI(a, "", ciPollIdlePane)
			scrollback := ""
			for range ciPollNudgeThreshold + 2 {
				scrollback += "gh run list --repo acme/widgets\n"
			}
			m.nudgeIfPollingCI(a, scrollback, ciPollIdlePane)

			if a.ciPollNudgeSent != tt.wantNudged {
				t.Errorf("ciPollNudgeSent = %v, want %v", a.ciPollNudgeSent, tt.wantNudged)
			}
			if a.AwaitingCI != tt.wantNudged {
				t.Errorf("AwaitingCI = %v, want %v", a.AwaitingCI, tt.wantNudged)
			}
			if (a.CIPollNudges > 0) != tt.wantNudged {
				t.Errorf("CIPollNudges = %d, wantNudged %v", a.CIPollNudges, tt.wantNudged)
			}
			if !tt.wantNudged {
				if sent != 0 {
					t.Errorf("sent %d literals to monitoring lane, want 0", sent)
				}
				if ev := ciPollEvidence(tr); len(ev) != 0 {
					t.Errorf("ci_poll_nudge evidence = %d for monitoring lane, want 0: %#v", len(ev), ev)
				}
			}
		})
	}
}

// TestNudgeIfPollingCISelfReportGate pins #13863: a single poll past the
// wall clock still gets the in-session nudge but produces no ci_poll_nudge
// fleet evidence; repeated polling produces exactly one.
func TestNudgeIfPollingCISelfReportGate(t *testing.T) {
	for _, tt := range []struct {
		name         string
		kickAge      time.Duration
		polls        int
		wantEvidence int
		sweepReads   int
	}{
		{name: "one poll past wall clock: nudged, not reported", kickAge: 11 * time.Minute, polls: 1, wantEvidence: 0},
		{name: "two polls past wall clock: nudged, not reported (#14327)", kickAge: 11 * time.Minute, polls: 2, wantEvidence: 0},
		{name: "repeated polls past wall clock: nudged and reported", kickAge: 11 * time.Minute, polls: ciPollReportMinPolls, wantEvidence: 1},
		{name: "sweep plus repeated target", kickAge: 11 * time.Minute, polls: ciPollReportMinPolls, sweepReads: 3, wantEvidence: 1},
		{name: "count threshold crossed: nudged and reported", kickAge: 2 * time.Minute, polls: ciPollNudgeThreshold + 1, wantEvidence: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, a := ciPollManager(t, ciPollIdlePane, tt.kickAge)
			tr := recordConditions(t, m)
			sent := 0
			termSeams(m).sendLiteral = func(*AgentProcess, string) { sent++ }

			m.nudgeIfPollingCI(a, "", ciPollIdlePane)
			scrollback := ""
			for i := 0; i < tt.polls; i++ {
				scrollback += "gh pr checks 123\n"
			}
			for i := 0; i < tt.sweepReads; i++ {
				scrollback += fmt.Sprintf("gh pr checks %d\n", 200+i)
			}
			m.nudgeIfPollingCI(a, scrollback, ciPollIdlePane)

			if !a.ciPollNudgeSent || sent != 1 {
				t.Fatalf("ciPollNudgeSent = %v, literals sent = %d, want nudge sent once", a.ciPollNudgeSent, sent)
			}
			ev := ciPollEvidence(tr)
			if len(ev) != tt.wantEvidence {
				t.Fatalf("ci_poll_nudge evidence = %d, want %d: %#v", len(ev), tt.wantEvidence, ev)
			}
			if tt.wantEvidence == 1 {
				if strings.Contains(ev[0].Message, "own PR") || !strings.Contains(ev[0].Message, fmt.Sprintf("%d repeated of %d", tt.polls, tt.polls+tt.sweepReads)) {
					t.Errorf("report must describe observed commands without inferring ownership: %s", ev[0].Message)
				}
				for _, want := range []string{"agent=scanner", fmt.Sprintf("polls_since_kick=%d", tt.polls+tt.sweepReads), fmt.Sprintf("report_min_polls=%d", ciPollReportMinPolls)} {
					if !strings.Contains(ev[0].Diagnostics, want) {
						t.Errorf("diagnostics missing %q:\n%s", want, ev[0].Diagnostics)
					}
				}
			}
		})
	}
}

func ciPollEvidence(rec *conditionRecorder) []conditionObservation {
	return rec.observations(crux.ClassCIPollNudge)
}
