package trajectory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	agents []AgentView
	paused []string
}

func (f *fakeSource) TrajectoryAgents() []AgentView { return f.agents }
func (f *fakeSource) Pause(name, trigger, reason string) error {
	f.paused = append(f.paused, name)
	return nil
}

type fakeSink struct {
	audits []string
	alerts []string
	notifs []string
}

func (f *fakeSink) Audit(agent, action, detail string) {
	f.audits = append(f.audits, agent+"/"+action)
}
func (f *fakeSink) Alert(id, severity, message string) {
	f.alerts = append(f.alerts, severity+":"+id)
}
func (f *fakeSink) Notify(title, message string) {
	f.notifs = append(f.notifs, title)
}

// verdictServer returns an httptest server that always replies with the given
// divergent value.
func verdictServer(t *testing.T, divergent bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		reason := "normal work"
		if divergent {
			reason = "sandbox escape attempt"
		}
		content := `{"divergent":` + boolStr(divergent) + `,"confidence":0.9,"reason":"` + reason + `"}`
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newTestLane(t *testing.T, src AgentSource, sink Sink, srvURL string, cfg LaneConfig) *Lane {
	t.Helper()
	reviewer, err := NewReviewer(Config{Endpoint: srvURL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return NewLane(reviewer, src, sink, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestLanePausesOnDivergence(t *testing.T) {
	srv := verdictServer(t, true)
	defer srv.Close()
	src := &fakeSource{agents: []AgentView{
		{Name: "scanner", Running: true, Intent: "fix the bug", Transcript: "splitting token"},
	}}
	sink := &fakeSink{}
	lane := newTestLane(t, src, sink, srv.URL, LaneConfig{OnDivergence: "pause"})
	lane.Run(context.Background())

	if len(src.paused) != 1 || src.paused[0] != "scanner" {
		t.Errorf("expected scanner paused, got %v", src.paused)
	}
	if len(sink.notifs) != 1 {
		t.Errorf("expected a notification, got %v", sink.notifs)
	}
	if len(sink.alerts) != 1 || !strings.HasPrefix(sink.alerts[0], "error:") {
		t.Errorf("expected error alert, got %v", sink.alerts)
	}
}

func TestLaneAlertOnlyDoesNotPause(t *testing.T) {
	srv := verdictServer(t, true)
	defer srv.Close()
	src := &fakeSource{agents: []AgentView{
		{Name: "reviewer", Running: true, Intent: "review", Transcript: "doing something"},
	}}
	sink := &fakeSink{}
	lane := newTestLane(t, src, sink, srv.URL, LaneConfig{OnDivergence: "alert"})
	lane.Run(context.Background())

	if len(src.paused) != 0 {
		t.Errorf("alert-only must not pause, got %v", src.paused)
	}
	if len(sink.alerts) != 1 || !strings.HasPrefix(sink.alerts[0], "warning:") {
		t.Errorf("expected warning alert, got %v", sink.alerts)
	}
}

func TestLaneSkipsNonDivergentAndExemptAndPaused(t *testing.T) {
	srv := verdictServer(t, false) // reviewer would say non-divergent anyway
	defer srv.Close()
	src := &fakeSource{agents: []AgentView{
		{Name: "ok", Running: true, Intent: "i", Transcript: "t"},
		{Name: "exempt", Running: true, Intent: "i", Transcript: "t"},
		{Name: "stopped", Running: false, Intent: "i", Transcript: "t"},
		{Name: "nointent", Running: true, Intent: "", Transcript: "t"},
	}}
	sink := &fakeSink{}
	lane := newTestLane(t, src, sink, srv.URL, LaneConfig{OnDivergence: "pause", ExemptAgents: []string{"exempt"}})
	lane.Run(context.Background())

	if len(src.paused) != 0 {
		t.Errorf("nothing should pause, got %v", src.paused)
	}
	if len(sink.audits) != 0 {
		t.Errorf("no audits expected, got %v", sink.audits)
	}
}

// TestLaneReviewErrorFailsOpen: a reviewer transport error must be logged and
// skipped, never pausing the agent (fail-open on outage).
func TestLaneReviewErrorFailsOpen(t *testing.T) {
	src := &fakeSource{agents: []AgentView{
		{Name: "scanner", Running: true, Intent: "fix the bug", Transcript: "splitting token"},
	}}
	sink := &fakeSink{}
	// Point the reviewer at an endpoint with no listener so the HTTP call
	// itself fails (transport error), exercising Run's error branch.
	reviewer, err := NewReviewer(Config{Endpoint: "http://127.0.0.1:1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	lane := NewLane(reviewer, src, sink, LaneConfig{OnDivergence: "pause"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.Run(context.Background())

	if len(src.paused) != 0 {
		t.Errorf("reviewer outage must not pause, got %v", src.paused)
	}
	if len(sink.audits) != 0 {
		t.Errorf("reviewer outage must not audit, got %v", sink.audits)
	}
}

// TestLanePauseFailureFallsThroughToAlert: when Pause itself errors, the
// divergence must still be surfaced via audit/alert/notify.
func TestLanePauseFailureFallsThroughToAlert(t *testing.T) {
	srv := verdictServer(t, true)
	defer srv.Close()
	src := &pauseFailSource{fakeSource: fakeSource{agents: []AgentView{
		{Name: "scanner", Running: true, Intent: "fix the bug", Transcript: "splitting token"},
	}}}
	sink := &fakeSink{}
	lane := newTestLane(t, src, sink, srv.URL, LaneConfig{OnDivergence: "pause"})
	lane.Run(context.Background())

	if len(sink.audits) != 1 || !strings.HasPrefix(sink.audits[0], "scanner/trajectory-pause") {
		t.Errorf("expected pause audit despite Pause error, got %v", sink.audits)
	}
	if len(sink.alerts) != 1 {
		t.Errorf("expected alert despite Pause error, got %v", sink.alerts)
	}
	if len(sink.notifs) != 1 {
		t.Errorf("expected notify despite Pause error, got %v", sink.notifs)
	}
}

type pauseFailSource struct {
	fakeSource
}

func (p *pauseFailSource) Pause(name, trigger, reason string) error {
	return errPauseBoom
}

var errPauseBoom = errors.New("pause boom")

// TestItoa covers zero, positive, negative, and multi-digit paths.
func TestItoa(t *testing.T) {
	cases := map[int]string{
		0:     "0",
		5:     "5",
		42:    "42",
		-7:    "-7",
		-1234: "-1234",
		999:   "999",
	}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestSanitize collapses newlines, carriage returns, and tabs to spaces.
func TestSanitize(t *testing.T) {
	in := "line one\nline two\r\nwith\ttab"
	want := "line one line two  with tab"
	if got := sanitize(in); got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
	if got := sanitize("no special chars"); got != "no special chars" {
		t.Errorf("sanitize should be identity when nothing to strip, got %q", got)
	}
}

func TestLaneDueGating(t *testing.T) {
	lane := newTestLane(t, &fakeSource{}, &fakeSink{}, "http://x", LaneConfig{IntervalS: 60})
	now := time.Now()
	if !lane.Due(now) {
		t.Error("first run should be due")
	}
	lane.Run(context.Background()) // sets lastRun
	if lane.Due(time.Now()) {
		t.Error("should not be due immediately after a run")
	}
	if !lane.Due(time.Now().Add(61 * time.Second)) {
		t.Error("should be due after the interval elapses")
	}
}
