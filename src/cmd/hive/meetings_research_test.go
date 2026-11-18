package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/research"
)

// fakeResearchRuntime satisfies research.Runtime.
type fakeResearchRuntime struct {
	mu       sync.Mutex
	added    map[string]config.AgentConfig
	observer func(crux.KickEvent)
	queued   []string
}

func (f *fakeResearchRuntime) EnqueueKick(agent, kickID, _ string) (string, error) {
	f.mu.Lock()
	f.queued = append(f.queued, kickID)
	obs := f.observer
	f.mu.Unlock()
	if obs != nil {
		obs(crux.KickEvent{Agent: agent, Event: crux.KickEventQueued, KickID: kickID})
	}
	return kickID, nil
}
func (f *fakeResearchRuntime) KickStatus(string) (crux.KickStatus, bool) {
	return crux.KickStatus{}, false
}
func (f *fakeResearchRuntime) KickQueueDepth(string) int    { return 0 }
func (f *fakeResearchRuntime) InFlightKickID(string) string { return "" }
func (f *fakeResearchRuntime) AddKickObserver(fn func(crux.KickEvent)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observer = fn
	return func() {}
}
func (f *fakeResearchRuntime) AddAgent(name string, cfg config.AgentConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[name] = cfg
}
func (f *fakeResearchRuntime) GetStatusFast(name string) (*crux.Process, error) {
	return &crux.Process{Name: name, State: crux.StateRunning}, nil
}
func (f *fakeResearchRuntime) Start(context.Context, string) error { return nil }

func (f *fakeResearchRuntime) emit(ev crux.KickEvent) {
	f.mu.Lock()
	obs := f.observer
	f.mu.Unlock()
	obs(ev)
}

func TestResearchBase(t *testing.T) {
	if got := researchBase(nil); got.Backend != "" {
		t.Fatal("nil config")
	}
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{"zeta": {Backend: "codex"}, "alpha": {Backend: "copilot", Model: "m1"}}
	if got := researchBase(cfg); got.Backend != "copilot" || got.Model != "m1" {
		t.Fatalf("first by name = %+v", got)
	}
	cfg.Agents["scanner"] = config.AgentConfig{Backend: "claude", Model: "m2", Mode: "ISSUES_AND_PRS"}
	if got := researchBase(cfg); got.Backend != "claude" || got.Model != "m2" || got.Mode != "" {
		t.Fatalf("scanner base = %+v", got)
	}
}

func TestWireMeetingsResearch(t *testing.T) {
	on := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, ResearchPoolSize: 3}}
	if wireMeetingsResearch(nil, nil, nil) != nil || wireMeetingsResearch(&config.Config{}, &fakeResearchRuntime{}, nil) != nil || wireMeetingsResearch(on, nil, nil) != nil {
		t.Fatal("off, nil config or nil runtime must leave research unwired")
	}
	if d := wireMeetingsResearch(on, struct{}{}, nil); d != nil {
		t.Fatal("an unsupported runtime must leave research unwired")
	}
	if d := wireMeetingsResearch(on, struct{}{}, discardLogger()); d != nil {
		t.Fatal("an unsupported runtime must leave research unwired")
	}
	t.Cleanup(func() { researchAgents.Store(nil) })
	rt := &fakeResearchRuntime{added: map[string]config.AgentConfig{}}
	d := wireMeetingsResearch(on, rt, discardLogger())
	if d == nil || len(rt.added) != 3 {
		t.Fatalf("pool %v, agents %v", d, rt.added)
	}
	if cfg := rt.added["research-1"]; cfg.Mode != research.Mode {
		t.Fatalf("research agents must be read-only: %+v", cfg)
	}
	kick, err := d.Dispatch(context.Background(), "what is vexa", map[string]string{"meeting_id": "mtg-1", "rid": "r1"})
	if err != nil || kick == "" {
		t.Fatalf("dispatch = %q, %v", kick, err)
	}
}

func TestResearchSignalMapping(t *testing.T) {
	cases := map[string]string{research.EventFinding: live.SignalFinding, research.EventDone: live.SignalComplete, research.EventFailed: live.SignalError,
		research.EventRunning: live.SignalRunning}
	for kind, want := range cases {
		sig, ok := researchSignal(research.Event{KickID: "k", Kind: kind, Text: "t"})
		if !ok || sig.Kind != want || sig.KickID != "k" || sig.Text != "t" {
			t.Errorf("%s -> %+v %v", kind, sig, ok)
		}
	}
	for _, kind := range []string{research.EventQueued, "other"} {
		if _, ok := researchSignal(research.Event{Kind: kind}); ok {
			t.Errorf("%s must not map", kind)
		}
	}
}

// fakeSpekForResearch is a Spek that never writes intents.
type fakeSpekForResearch struct {
	lines chan []byte
}

func (s *fakeSpekForResearch) Start(context.Context) error { return nil }
func (s *fakeSpekForResearch) Send(any) error              { return nil }
func (s *fakeSpekForResearch) SendLine([]byte) error       { return nil }
func (s *fakeSpekForResearch) Lines() <-chan []byte        { return s.lines }
func (s *fakeSpekForResearch) Close(any)                   {}

func TestRouteResearch(t *testing.T) {
	t.Cleanup(func() { researchAgents.Store(nil) })
	if remove := routeResearch(nil, nil); remove == nil {
		t.Fatal("always returns a func")
	}
	on := &config.Config{Meetings: config.MeetingsConfig{Enabled: true}}
	rt := &fakeResearchRuntime{added: map[string]config.AgentConfig{}}
	d := wireMeetingsResearch(on, rt, nil)
	if remove := routeResearch(d, nil); remove == nil {
		t.Fatal("nil runner")
	}
	if remove := routeResearch(errDispatcher{}, &live.Runner{}); remove == nil {
		t.Fatal("non-observer dispatcher")
	}
	lr := newResearchTestRunner(t, d)
	remove := routeResearch(d, lr)
	defer remove()
	kick, _ := d.Dispatch(context.Background(), "q", map[string]string{"meeting_id": "mtg-x", "rid": "r"})
	// Unknown kicks to the live loop are dropped without blocking.
	rt.emit(crux.KickEvent{KickID: kick, Event: crux.KickEventOutputSignal, Signal: crux.OutputSignalFinding, Text: "f"})
	rt.emit(crux.KickEvent{KickID: kick, Event: crux.KickEventCompleted})
}

type errDispatcher struct{}

func (errDispatcher) Dispatch(context.Context, string, map[string]string) (string, error) {
	return "", errors.New("no")
}

func newResearchTestRunner(t *testing.T, d live.ResearchDispatcher) *live.Runner {
	t.Helper()
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	lr, err := live.NewRunner(live.Options{
		Meetings: mgr, Store: store, Research: d,
		NewSpek: func(meetings.Meeting) live.Spek { return &fakeSpekForResearch{lines: make(chan []byte)} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lr.Close)
	return lr
}

// TestWithResearchAgentsSurvivesReconcile pins hivecommons/hive#18590
// finding 6: the research agents are runtime-only, so the reconcile set
// must carry them or a config reload removes them.
func TestWithResearchAgentsSurvivesReconcile(t *testing.T) {
	t.Cleanup(func() { researchAgents.Store(nil) })
	researchAgents.Store(nil)
	cfgs := map[string]config.AgentConfig{"scanner": {Backend: "claude"}}
	if got := withResearchAgents(cfgs); len(got) != 1 {
		t.Fatalf("unwired pool adds nothing: %v", got)
	}
	on := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, ResearchPoolSize: 2}, AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: cfgs}}
	if wireMeetingsResearch(on, &fakeResearchRuntime{added: map[string]config.AgentConfig{}}, nil) == nil {
		t.Fatal("pool not wired")
	}
	got := withResearchAgents(cfgs)
	if len(got) != 3 || got["research-1"].Mode != research.Mode || got["research-2"].Role != research.Role || got["scanner"].Backend != "claude" {
		t.Fatalf("reconcile set = %+v", got)
	}
	if len(cfgs) != 1 {
		t.Fatal("the caller's map is not modified")
	}
	// A configured agent of the same name wins.
	cfgs["research-1"] = config.AgentConfig{Backend: "copilot"}
	if withResearchAgents(cfgs)["research-1"].Backend != "copilot" {
		t.Fatal("hive.yaml wins over the pool's default")
	}
	if got := withResearchAgents(nil); len(got) != 2 {
		t.Fatalf("nil configs = %v", got)
	}
}

func TestMeetingsResearchStatus(t *testing.T) {
	if meetingsResearchStatus(nil, nil) != nil {
		t.Fatal("no pool, no report")
	}
	t.Cleanup(func() { researchAgents.Store(nil) })
	cfg := &config.Config{Meetings: config.MeetingsConfig{Enabled: true, ResearchPoolSize: 2},
		AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"scanner": {Backend: "claude", Model: "m1"}, "a": {Backend: "codex"}}}}
	d := wireMeetingsResearch(cfg, &fakeResearchRuntime{added: map[string]config.AgentConfig{}}, nil)
	fn := meetingsResearchStatus(cfg, d)
	if fn == nil {
		t.Fatal("a pool reports")
	}
	st := fn()
	if len(st.Agents) != 2 || st.Available != 2 || st.BaseAgent != "scanner" || st.Backend != "claude" || st.Model != "m1" {
		t.Fatalf("status = %+v", st)
	}
	if got := researchBaseName(&config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"b": {}, "a": {}}}}); got != "a" {
		t.Fatalf("first by name, got %q", got)
	}
	if researchBaseName(nil) != "" {
		t.Fatal("nil config")
	}
}
