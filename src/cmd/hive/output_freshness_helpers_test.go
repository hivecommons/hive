package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// These tests pin the pure heartbeat helpers added for the output-freshness
// signal (#6048) and the restart-storm snapshot round-trip (#6039), all of
// which previously had no direct coverage:
//
//   - agentCanProduceJudgedOutput — the ACMM capability gate
//   - outputFreshnessHeartbeatFields — disposition/reason state machine
//   - restartEventsToSnapshot / restartEventsFromSnapshot — 24h cutoff filters
//   - firstNonEmpty — whitespace-skipping first-value picker

func TestAgentCanProduceJudgedOutput(t *testing.T) {
	all := hub.AgentSummary{CanOpenIssue: true, CanOpenPR: true, CanMerge: true}
	issueOnly := hub.AgentSummary{CanOpenIssue: true}
	prOnly := hub.AgentSummary{CanOpenPR: true}
	mergeOnly := hub.AgentSummary{CanMerge: true}
	none := hub.AgentSummary{}

	cases := []struct {
		name  string
		level int
		agent hub.AgentSummary
		want  bool
	}{
		{"level 0 never judged", 0, all, false},
		{"level 2 advisory band never judged", 2, all, false},
		{"level 3 issue capability counts", 3, issueOnly, true},
		{"level 3 pr capability counts", 3, prOnly, true},
		{"level 5 merge-only does not count in issue/pr band", 5, mergeOnly, false},
		{"level 5 no capabilities", 5, none, false},
		{"level 6 requires merge", 6, mergeOnly, true},
		{"level 6 issue/pr without merge does not count", 6, issueOnly, false},
		{"level 7 merge counts", 7, all, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCanProduceJudgedOutput(tc.level, tc.agent); got != tc.want {
				t.Errorf("agentCanProduceJudgedOutput(%d, %+v) = %v, want %v",
					tc.level, tc.agent, got, tc.want)
			}
		})
	}
}

func TestOutputFreshnessDispositionPrecedence(t *testing.T) {
	writer := hub.AgentSummary{Name: "quality", CanOpenPR: true}

	cases := []struct {
		name            string
		acmmLevel       int
		gov             governor.State
		agents          []hub.AgentSummary
		wantDisposition string
	}{
		{
			name:            "advisory band wins even with queued work",
			acmmLevel:       2,
			gov:             governor.State{QueueIssues: 3, BudgetExhausted: true},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "advisory-only",
		},
		{
			name:            "budget exhaustion beats queue state",
			acmmLevel:       4,
			gov:             governor.State{QueueIssues: 3, BudgetExhausted: true},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "budget-suppressed",
		},
		{
			name:            "held-only queue is agent-decided-not-writable",
			acmmLevel:       4,
			gov:             governor.State{QueueHold: 2},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "agent-decided-not-writable",
		},
		{
			name:            "empty queue is idle",
			acmmLevel:       4,
			gov:             governor.State{},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "idle",
		},
		{
			name:            "queued work but no cadences means no due agents",
			acmmLevel:       4,
			gov:             governor.State{QueueIssues: 1},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "no-due-agents",
		},
		{
			name:      "due interval cadence on a write-capable agent is kick-capable",
			acmmLevel: 4,
			gov: governor.State{
				QueueIssues: 1,
				Cadences: map[string]governor.AgentCadence{
					"quality": {Agent: "quality", Interval: time.Hour},
				},
				LastKick: map[string]time.Time{},
			},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "kick-capable",
		},
		{
			name:      "paused cadence is not due",
			acmmLevel: 4,
			gov: governor.State{
				QueueIssues: 1,
				Cadences: map[string]governor.AgentCadence{
					"quality": {Agent: "quality", Interval: time.Hour, Paused: true},
				},
				LastKick: map[string]time.Time{},
			},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "no-due-agents",
		},
		{
			name:      "recently kicked interval agent is not due",
			acmmLevel: 4,
			gov: governor.State{
				QueueIssues: 1,
				Cadences: map[string]governor.AgentCadence{
					"quality": {Agent: "quality", Interval: time.Hour},
				},
				LastKick: map[string]time.Time{"quality": time.Now().Add(-time.Minute)},
			},
			agents:          []hub.AgentSummary{writer},
			wantDisposition: "no-due-agents",
		},
		{
			name:      "cadence on a non-write-capable agent does not make the hive kick-capable",
			acmmLevel: 4,
			gov: governor.State{
				QueueIssues: 1,
				Cadences: map[string]governor.AgentCadence{
					"brainstorm": {Agent: "brainstorm", Interval: time.Hour},
				},
				LastKick: map[string]time.Time{},
			},
			agents:          []hub.AgentSummary{{Name: "brainstorm"}},
			wantDisposition: "no-due-agents",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, disposition, reason, _ := outputFreshnessHeartbeatFields(tc.acmmLevel, tc.gov, tc.agents)
			if disposition != tc.wantDisposition {
				t.Errorf("disposition = %q, want %q", disposition, tc.wantDisposition)
			}
			if reason == "" {
				t.Error("reason must never be empty: every disposition carries an operator explanation")
			}
		})
	}
}

func TestOutputFreshnessLastWriteKickAt(t *testing.T) {
	older := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC)
	newest := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)

	gov := governor.State{
		QueueHold: 3,
		LastKick: map[string]time.Time{
			"quality":    older,
			"scanner":    newer,
			"brainstorm": newest, // most recent kick, but agent cannot write
		},
	}
	agents := []hub.AgentSummary{
		{Name: "quality", CanOpenPR: true},
		{Name: "scanner", CanOpenIssue: true},
		{Name: "brainstorm"}, // advisory-only: no capabilities
	}

	lastKick, _, _, notWritableQueued := outputFreshnessHeartbeatFields(4, gov, agents)

	// The freshness clock tracks JUDGED output: brainstorm's newest kick must
	// be ignored because it cannot open issues or PRs at level 4.
	if want := newer.Format(time.RFC3339); lastKick != want {
		t.Errorf("lastWriteKickAt = %q, want %q (newest kick among write-capable agents)", lastKick, want)
	}
	if notWritableQueued != 3 {
		t.Errorf("notWritableQueued = %d, want 3 (mirrors govState.QueueHold)", notWritableQueued)
	}
}

func TestOutputFreshnessNoKicksYieldsEmptyTimestamp(t *testing.T) {
	lastKick, _, _, _ := outputFreshnessHeartbeatFields(4, governor.State{}, []hub.AgentSummary{
		{Name: "quality", CanOpenPR: true},
	})
	if lastKick != "" {
		t.Errorf("lastWriteKickAt = %q, want empty when no write-capable agent was ever kicked", lastKick)
	}
}

func TestRestartEventsToSnapshotFiltersStaleAndZero(t *testing.T) {
	now := time.Now()
	events := []agent.RestartEvent{
		{At: now.Add(-25 * time.Hour), Reason: "too old"},
		{}, // zero timestamp
		{At: now.Add(-time.Hour), Reason: "recent"},
		{At: now.Add(-23 * time.Hour), Reason: "just inside window"},
	}
	got := restartEventsToSnapshot(events)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (24h cutoff drops stale and zero entries): %+v", len(got), got)
	}
	if got[0].Reason != "recent" || got[1].Reason != "just inside window" {
		t.Errorf("kept events out of order or wrong: %+v", got)
	}
}

func TestRestartEventsSnapshotRoundTripEmpty(t *testing.T) {
	if got := restartEventsToSnapshot(nil); got != nil {
		t.Errorf("restartEventsToSnapshot(nil) = %+v, want nil", got)
	}
	if got := restartEventsFromSnapshot(nil); got != nil {
		t.Errorf("restartEventsFromSnapshot(nil) = %+v, want nil", got)
	}
	// All-stale input must also yield an EMPTY result, not a non-nil slice of
	// ghosts that would repopulate restart-storm counters after a reload.
	stale := []snapshot.AgentRestartEvent{{At: time.Now().Add(-48 * time.Hour), Reason: "ancient"}}
	if got := restartEventsFromSnapshot(stale); len(got) != 0 {
		t.Errorf("restartEventsFromSnapshot(stale) = %+v, want empty", got)
	}
}

func TestRestartEventsFromSnapshotKeepsRecent(t *testing.T) {
	now := time.Now()
	snap := []snapshot.AgentRestartEvent{
		{At: now.Add(-30 * time.Hour), Reason: "stale"},
		{At: now.Add(-5 * time.Minute), Reason: "provider-error"},
	}
	got := restartEventsFromSnapshot(snap)
	if len(got) != 1 || got[0].Reason != "provider-error" {
		t.Fatalf("got %+v, want the single recent event with its reason preserved", got)
	}
	if !got[0].At.Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("timestamp not preserved: %v", got[0].At)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"skips whitespace-only values", []string{"", "   ", "\t\n", "found"}, "found"},
		{"returns the untrimmed original", []string{"", " padded "}, " padded "},
		{"all blank yields empty", []string{"", "  ", ""}, ""},
		{"no args yields empty", nil, ""},
		{"first wins", []string{"a", "b"}, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNonEmpty(tc.in...); got != tc.want {
				t.Errorf("firstNonEmpty(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
