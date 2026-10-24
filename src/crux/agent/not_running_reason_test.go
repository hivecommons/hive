package agent

import (
	"strings"
	"testing"
	"time"
)

// TestNotRunningReasonNamesTheState guards the wording fix. The old message,
// "agent <name> not running", came from a single State != StateRunning check
// and read identically for an agent the operator had deliberately paused, one
// that failed to launch, and one that was never started. During a live incident
// it was read as a launch failure on agents that were paused exactly as
// intended.
func TestNotRunningReasonNamesTheState(t *testing.T) {
	cases := []struct {
		name  string
		agent *AgentProcess
		want  []string
		// deny lists substrings that would reintroduce the ambiguity.
		deny []string
	}{
		{
			name: "operator paused via dashboard",
			agent: &AgentProcess{
				State: StatePaused, Paused: true,
				PausedTrigger: "dashboard-api",
				PausedReason:  "operator request",
			},
			want: []string{"paused", "dashboard-api", "operator request"},
		},
		{
			name:  "paused with no trigger recorded",
			agent: &AgentProcess{State: StatePaused, Paused: true},
			want:  []string{"paused"},
		},
		{
			// Paused before Start() ran, so State still holds the old value.
			// The message must still say "paused", not "stopped".
			name:  "paused flag set while state lags",
			agent: &AgentProcess{State: StateStopped, Paused: true},
			want:  []string{"paused"},
			deny:  []string{"stopped"},
		},
		{
			name:  "failed to launch, with cause",
			agent: &AgentProcess{State: StateFailed, LastError: "missing API key"},
			want:  []string{"failed to start", "missing API key"},
			deny:  []string{"paused"},
		},
		{
			name:  "stopped",
			agent: &AgentProcess{State: StateStopped},
			want:  []string{"stopped"},
			deny:  []string{"paused"},
		},
		{
			name:  "never started",
			agent: &AgentProcess{State: StateIdle},
			want:  []string{"not been started"},
			deny:  []string{"paused"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := notRunningReason(tc.agent)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("reason %q does not mention %q", got, w)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("reason %q wrongly mentions %q", got, d)
				}
			}
			// The whole point of the change: never the bare old phrasing.
			if got == "not running" {
				t.Errorf("reason fell back to the ambiguous original wording")
			}
		})
	}
}

// TestSamePaneCapture covers the comparison that drives the activity clock: an
// agent producing nothing re-captures a byte-identical pane, and that is what
// must NOT advance the timestamp.
func TestSamePaneCapture(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"one", "two"}, []string{"one", "two"}, true},
		{"both empty", nil, nil, true},
		{"content differs", []string{"one", "two"}, []string{"one", "three"}, false},
		{"length differs", []string{"one"}, []string{"one", "two"}, false},
		{"empty vs populated", nil, []string{"one"}, false},
		{"order differs", []string{"a", "b"}, []string{"b", "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := samePaneCapture(tc.a, tc.b); got != tc.want {
				t.Errorf("samePaneCapture(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestSnapshotCarriesLastPaneChange checks the field survives the snapshot that
// feeds both the dashboard and the heartbeat — the same hop where StartedAt was
// being dropped (#2324).
func TestSnapshotCarriesLastPaneChange(t *testing.T) {
	when := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	a := &AgentProcess{
		Name:           "scanner",
		State:          StateRunning,
		StartedAt:      &started,
		LastPaneChange: when,
		OutputBuffer:   NewRingBuffer(outputBufferCapacity),
	}
	snap := a.snapshot()
	if !snap.LastPaneChange.Equal(when) {
		t.Errorf("LastPaneChange = %v, want %v", snap.LastPaneChange, when)
	}
	if snap.StartedAt == nil || !snap.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", snap.StartedAt, started)
	}
}
