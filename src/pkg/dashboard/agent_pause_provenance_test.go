package dashboard

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
)

// fakePauseSource answers from the runtime facts it is handed: a paused agent
// gets a provenance and a refusal line, a running one gets neither. The wording
// and classification are pkg/pauseprov's tests; here only the wiring is.
type fakePauseSource struct{ resumes []string }

func (f *fakePauseSource) Describe(agent string, paused bool, trigger, by, reason string, at time.Time) (any, string) {
	if !paused {
		return nil, ""
	}
	return map[string]any{"pause": map[string]any{"by": by, "trigger": trigger}},
		"agent " + agent + " cannot be kicked: it is paused by " + by + ": " + reason + "; resume here"
}

func (f *fakePauseSource) IntendResume(agent, by string) { f.resumes = append(f.resumes, agent+"|"+by) }

func provServer(t *testing.T) (*Server, *cruxtest.Recorder, *fakePauseSource) {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	rt := cruxtest.NewRecorder()
	rt.Add("a", "a", crux.StateIdle)
	deps := testDeps(t)
	deps.AgentMgr = rt
	s.RegisterAPI(deps)
	f := &fakePauseSource{}
	SetPauseProvenanceSource(f)
	t.Cleanup(func() { SetPauseProvenanceSource(nil) })
	return s, rt, f
}

func TestPausedKickMessageUsesProvenance(t *testing.T) {
	s, rt, _ := provServer(t)
	_ = rt.PauseBy("a", "dashboard-api", "manual pause", "alice")
	msg := s.pausedKickMessage("a", "agent a cannot be kicked: it is paused (by dashboard-api): manual pause")
	want := "agent a cannot be kicked: it is paused by alice: manual pause; resume here"
	if msg != want {
		t.Fatalf("%q", msg)
	}
	if classifyKickError(msg) != kickRefusalPaused {
		t.Fatal("the rewritten message no longer classifies as paused, so its docs link is lost")
	}
	if got := s.pausedKickMessage("a", "agent a cannot be kicked: it is stopped"); got != "agent a cannot be kicked: it is stopped" {
		t.Fatalf("%q", got)
	}
	_ = rt.Resume(context.Background(), "a", "dashboard-api", "manual resume")
	orig := "agent a cannot be kicked: it is paused"
	if got := s.pausedKickMessage("a", orig); got != orig {
		t.Fatalf("a running agent was described as paused: %q", got)
	}
}

func TestPauseProvenanceOnTheStatusCard(t *testing.T) {
	_, _, _ = provServer(t)
	paused := &crux.Process{Paused: true, PausedBy: "alice"}
	agents := []FrontendAgent{{Name: "a"}, {Name: "b"}}
	applyPauseProvenance(agents, map[string]*crux.Process{"a": paused, "b": {}})
	if agents[0].PauseProvenance == nil || agents[1].PauseProvenance != nil {
		t.Fatalf("%+v", agents)
	}
	if p, r := agentPauseProvenance("a", nil); p != nil || r != "" {
		t.Fatal("nil status")
	}
}

func TestNoSourceMeansNoProvenance(t *testing.T) {
	SetPauseProvenanceSource(nil)
	if p, r := agentPauseProvenance("a", &crux.Process{Paused: true}); p != nil || r != "" {
		t.Fatal("provenance without a source")
	}
}
