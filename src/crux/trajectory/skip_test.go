package trajectory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer is a non-divergent reviewer that counts requests.
func countingServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		io.Copy(io.Discard, r.Body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"divergent":false,"confidence":0.9,"reason":"ok"}`}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestSkipDecision pins the predicate table (hivecommons/hive#12132).
func TestSkipDecision(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	interval := 10 * time.Minute
	maxSkip := interval * DefaultMaxSkipIntervals
	reviewed := AgentReviewState{LastReviewed: t0}
	cases := []struct {
		name   string
		a      AgentView
		st     AgentReviewState
		now    time.Time
		skip   bool
		reason string
	}{
		{"never reviewed always runs", AgentView{Idle: true, LastOutputAt: t0.Add(-time.Hour)}, AgentReviewState{}, t0.Add(interval), false, ReasonFirstReview},
		{"idle, no new output, not overdue -> skip", AgentView{Idle: true, LastOutputAt: t0.Add(-time.Minute)}, reviewed, t0.Add(interval), true, ReasonIdle},
		{"idle but overdue -> runs", AgentView{Idle: true, LastOutputAt: t0.Add(-time.Minute)}, reviewed, t0.Add(maxSkip), false, ReasonOverdue},
		{"running (turn in progress) with stale output -> runs", AgentView{Idle: false, LastOutputAt: t0.Add(-time.Hour)}, reviewed, t0.Add(interval), false, ReasonActive},
		{"idle with new output since review -> runs", AgentView{Idle: true, LastOutputAt: t0.Add(time.Minute)}, reviewed, t0.Add(interval), false, ReasonNewOutput},
		{"idle but output time unknown -> runs", AgentView{Idle: true}, reviewed, t0.Add(interval), false, ReasonNewOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, reason := skipDecision(tc.a, tc.st, tc.now, maxSkip)
			if skip != tc.skip || reason != tc.reason {
				t.Fatalf("skipDecision = (%v, %q), want (%v, %q)", skip, reason, tc.skip, tc.reason)
			}
		})
	}
}

// TestLaneStalledButRunningAgentIsStillReviewed is the hard constraint from
// ADR-0009: a byte-identical transcript on an agent whose turn is still in
// progress must be reviewed on every pass — the stall may be the signal.
func TestLaneStalledButRunningAgentIsStillReviewed(t *testing.T) {
	srv, calls := countingServer(t)
	stale := time.Now().Add(-2 * time.Hour)
	src := &fakeSource{agents: []AgentView{
		{Name: "scanner", Running: true, Idle: false, Intent: "fix", Transcript: "same bytes every time", LastOutputAt: stale},
	}}
	lane := newTestLane(t, src, &fakeSink{}, srv.URL, LaneConfig{IntervalS: 600})
	now := time.Now()
	for i := 0; i < 4; i++ {
		lane.runAt(context.Background(), now.Add(time.Duration(i)*10*time.Minute))
	}
	if got := atomic.LoadInt32(calls); got != 4 {
		t.Fatalf("stalled-but-running agent reviewed %d times over 4 passes, want 4", got)
	}
	st := lane.Stats()
	if st.Skipped != 0 || st.Reviewed != 4 || st.RanByReason[ReasonActive] != 3 || st.RanByReason[ReasonFirstReview] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestLaneIdleNotOverdueAgentIsSkipped: a proven-idle agent with no output
// since its last review is skipped, with a recorded reason, until overdue.
func TestLaneIdleNotOverdueAgentIsSkipped(t *testing.T) {
	srv, calls := countingServer(t)
	now := time.Now()
	src := &fakeSource{agents: []AgentView{
		{Name: "idle", Running: true, Idle: true, Intent: "fix", Transcript: "done", LastOutputAt: now.Add(-time.Minute)},
	}}
	lane := newTestLane(t, src, &fakeSink{}, srv.URL, LaneConfig{IntervalS: 600, MaxSkipIntervals: 3})

	lane.runAt(context.Background(), now) // first review always runs
	lane.runAt(context.Background(), now.Add(10*time.Minute))
	lane.runAt(context.Background(), now.Add(20*time.Minute))
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("idle agent reviewed %d times, want 1 (two skips)", got)
	}
	st := lane.Stats()
	if st.Skipped != 2 || st.SkippedByReason[ReasonIdle] != 2 {
		t.Fatalf("stats = %+v, want 2 idle skips", st)
	}
	if a := st.Agents["idle"]; a.LastDecision != "skipped" || a.LastReason != ReasonIdle || a.Skipped != 2 {
		t.Fatalf("agent state = %+v", a)
	}

	// Overdue: 3 intervals since the last review → runs regardless of idleness.
	lane.runAt(context.Background(), now.Add(30*time.Minute))
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("overdue idle agent reviewed %d times total, want 2", got)
	}
	st = lane.Stats()
	if st.RanByReason[ReasonOverdue] != 1 {
		t.Fatalf("expected one overdue run, stats = %+v", st)
	}
	if a := st.Agents["idle"]; a.LastDecision != "reviewed" || a.Skipped != 0 {
		t.Fatalf("agent state after overdue run = %+v", a)
	}
}

// TestLaneIdleAgentWithNewOutputIsReviewed: idleness alone is not enough —
// output produced since the last review re-arms the review.
func TestLaneIdleAgentWithNewOutputIsReviewed(t *testing.T) {
	srv, calls := countingServer(t)
	now := time.Now()
	a := AgentView{Name: "a", Running: true, Idle: true, Intent: "fix", Transcript: "x", LastOutputAt: now.Add(-time.Minute)}
	src := &fakeSource{agents: []AgentView{a}}
	lane := newTestLane(t, src, &fakeSink{}, srv.URL, LaneConfig{IntervalS: 600})
	lane.runAt(context.Background(), now)
	a.LastOutputAt = now.Add(5 * time.Minute)
	src.agents = []AgentView{a}
	lane.runAt(context.Background(), now.Add(10*time.Minute))
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (new output re-arms review)", got)
	}
	if st := lane.Stats(); st.RanByReason[ReasonNewOutput] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestLaneUnknownOutputTimeNeverSkips: an Idle view without a known
// last-output time is not proven idle, so it is reviewed.
func TestLaneUnknownOutputTimeNeverSkips(t *testing.T) {
	srv, calls := countingServer(t)
	now := time.Now()
	src := &fakeSource{agents: []AgentView{{Name: "a", Running: true, Idle: true, Intent: "fix", Transcript: "x"}}}
	lane := newTestLane(t, src, &fakeSink{}, srv.URL, LaneConfig{IntervalS: 600})
	lane.runAt(context.Background(), now)
	lane.runAt(context.Background(), now.Add(10*time.Minute))
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestLaneStatsNilSafeAndCopied(t *testing.T) {
	var nilLane *Lane
	if st := nilLane.Stats(); st.Reviewed != 0 {
		t.Fatal("nil lane stats should be zero")
	}
	lane := newTestLane(t, &fakeSource{}, &fakeSink{}, "http://x", LaneConfig{})
	st := lane.Stats()
	st.SkippedByReason["x"] = 1
	if lane.Stats().SkippedByReason["x"] != 0 {
		t.Fatal("Stats must return a copy")
	}
}
