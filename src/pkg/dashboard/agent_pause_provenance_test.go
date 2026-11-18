package dashboard

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/pauseprov"
)

func provServer(t *testing.T) (*Server, *cruxtest.Recorder) {
	t.Helper()
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	rt := cruxtest.NewRecorder()
	rt.Add("a", "a", crux.StateIdle)
	deps := testDeps(t)
	deps.AgentMgr = rt
	s.RegisterAPI(deps)
	return s, rt
}

func TestPausedKickMessageNamesWhoWhenWhy(t *testing.T) {
	s, rt := provServer(t)
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	rt.Procs["a"].PausedAt = at
	_ = rt.PauseBy("a", "dashboard-api", "manual pause", "alice")
	msg := s.pausedKickMessage("a", "agent a cannot be kicked: it is paused (by dashboard-api): manual pause")
	want := "agent a cannot be kicked: it is paused by dashboard user alice at 2026-11-17T12:00:00Z: manual pause; resume here"
	if msg != want {
		t.Fatalf("%q", msg)
	}
	if classifyKickError(msg) != kickRefusalPaused {
		t.Fatal("the rewritten message no longer classifies as paused, so its docs link is lost")
	}
	// Another refusal, or an agent that is not paused, is untouched.
	if got := s.pausedKickMessage("a", "agent a cannot be kicked: it is stopped"); got != "agent a cannot be kicked: it is stopped" {
		t.Fatalf("%q", got)
	}
	_ = rt.Resume(context.Background(), "a", "dashboard-api", "manual resume")
	if got := s.pausedKickMessage("a", "agent a cannot be kicked: it is paused"); strings.Contains(got, "resume here") {
		t.Fatalf("a running agent was described as paused: %q", got)
	}
}

func TestPauseProvenanceOnTheStatusCard(t *testing.T) {
	st, _ := pauseprov.Open("", nil)
	prev := pauseprov.Default()
	pauseprov.SetDefault(st)
	defer pauseprov.SetDefault(prev)
	st.ObserveResume("a", pauseprov.Event{Trigger: "provider-rotation", Reason: "provider headroom recovered", At: time.Now()})

	paused := &crux.Process{Paused: true}
	p := agentPauseProvenance("a", paused)
	if p == nil || p.Pause == nil || p.Pause.Kind != pauseprov.KindUnknown || p.LastResume == nil || p.LastResume.Kind != pauseprov.KindAutoRateLimit {
		t.Fatalf("a pause with no recorded provenance must read as unknown, with the last resume kept: %+v", p)
	}
	agents := []FrontendAgent{{Name: "a"}, {Name: "b"}}
	applyPauseProvenance(agents, map[string]*crux.Process{"a": paused, "b": {}})
	if agents[0].PauseProvenance == nil || agents[1].PauseProvenance != nil {
		t.Fatalf("%+v", agents)
	}
	if agentPauseProvenance("a", nil) != nil {
		t.Fatal("nil status")
	}
}
