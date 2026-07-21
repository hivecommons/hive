package trajectory

import (
	"context"
	"encoding/json"
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
