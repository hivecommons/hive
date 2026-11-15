package research

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

type fakeRuntime struct {
	added    map[string]config.AgentConfig
	states   map[string]*crux.Process
	depth    map[string]int
	inflight map[string]string
	started  []string
	startErr error
	queueErr error
	queued   []string // agent:kickID
	prompts  []string
	observer func(crux.KickEvent)
}

func newFake(names ...string) *fakeRuntime {
	f := &fakeRuntime{
		added:    map[string]config.AgentConfig{},
		states:   map[string]*crux.Process{},
		depth:    map[string]int{},
		inflight: map[string]string{},
	}
	for _, n := range names {
		f.states[n] = &crux.Process{Name: n, State: crux.StateRunning}
	}
	return f
}

func (f *fakeRuntime) EnqueueKick(agent, kickID, message string) (string, error) {
	if f.queueErr != nil {
		return "", f.queueErr
	}
	f.queued = append(f.queued, agent+":"+kickID)
	f.prompts = append(f.prompts, message)
	f.depth[agent]++
	if f.observer != nil {
		f.observer(crux.KickEvent{Agent: agent, Event: crux.KickEventQueued, KickID: kickID})
	}
	return kickID, nil
}
func (f *fakeRuntime) KickStatus(string) (crux.KickStatus, bool) { return crux.KickStatus{}, false }
func (f *fakeRuntime) KickQueueDepth(agent string) int           { return f.depth[agent] }
func (f *fakeRuntime) InFlightKickID(agent string) string        { return f.inflight[agent] }
func (f *fakeRuntime) AddKickObserver(fn func(crux.KickEvent)) func() {
	f.observer = fn
	return func() { f.observer = nil }
}
func (f *fakeRuntime) AddAgent(name string, cfg config.AgentConfig) { f.added[name] = cfg }
func (f *fakeRuntime) GetStatusFast(name string) (*crux.Process, error) {
	st, ok := f.states[name]
	if !ok {
		return nil, errors.New("agent not found")
	}
	return st, nil
}
func (f *fakeRuntime) Start(_ context.Context, name string) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, name)
	f.states[name].State = crux.StateRunning
	return nil
}

func TestPoolSizeAndNames(t *testing.T) {
	for in, want := range map[int]int{-1: DefaultPoolSize, 0: DefaultPoolSize, 3: 3, MaxPoolSize + 5: MaxPoolSize} {
		if got := PoolSize(in); got != want {
			t.Errorf("PoolSize(%d) = %d, want %d", in, got, want)
		}
	}
	if got := strings.Join(AgentNames(3), ","); got != "research-1,research-2,research-3" {
		t.Errorf("AgentNames(3) = %s", got)
	}
	for name, want := range map[string]bool{"research-1": true, "research-12": true, "research-0": false, "research-x": false, "research": false, "scanner": false} {
		if got := IsResearchAgent(name); got != want {
			t.Errorf("IsResearchAgent(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAgentConfigForcesReadOnly(t *testing.T) {
	yes := true
	cfg := AgentConfig(config.AgentConfig{Backend: "claude", Model: "m", Mode: "ISSUES_AND_PRS", IncludeRepos: &yes, Converse: &yes, Role: "scanner"})
	if cfg.Mode != Mode || cfg.Role != Role || cfg.IncludeRepos == nil || *cfg.IncludeRepos || !cfg.OnDemand || cfg.Converse != nil {
		t.Fatalf("read-only fields not forced: %+v", cfg)
	}
	if cfg.Backend != "claude" || cfg.Model != "m" {
		t.Fatalf("base backend/model not kept: %+v", cfg)
	}
	if cfg.DisplayName == "" || cfg.Emoji == "" || cfg.Description == "" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	kept := AgentConfig(config.AgentConfig{DisplayName: "d", Emoji: "e", Description: "x"})
	if kept.DisplayName != "d" || kept.Emoji != "e" || kept.Description != "x" {
		t.Fatalf("base labels overwritten: %+v", kept)
	}
	if m, ok := crux.ParseAgentMode(Mode); !ok || m != crux.ModeAdvisory {
		t.Fatalf("Mode %q is not ADVISORY", Mode)
	}
}

func TestPromptNeverStartsALineWithAMarker(t *testing.T) {
	p := Prompt("  what changed in vLLM 0.9?  ")
	if !strings.HasSuffix(p, "Question: what changed in vLLM 0.9?") {
		t.Fatalf("prompt does not end with the query: %q", p)
	}
	if strings.Contains(p, "[FINDING]") || strings.Contains(p, "[COMPLETE]") || strings.Contains(p, "[ERROR]") {
		t.Fatal("prompt spells a literal marker; a wrapped echo could be read as a signal")
	}
	long := Prompt(strings.Repeat("é", maxQueryLen))
	q := long[strings.LastIndex(long, "Question: ")+len("Question: "):]
	if len(q) > maxQueryLen || !strings.HasPrefix(q, "é") || strings.ContainsRune(q, '�') {
		t.Fatalf("long query not truncated cleanly: %d bytes", len(q))
	}
}

func TestNewPoolRejectsUnsupportedRuntime(t *testing.T) {
	if _, err := NewPool(struct{}{}, 2); !errors.Is(err, ErrUnsupportedRuntime) {
		t.Fatalf("err = %v, want ErrUnsupportedRuntime", err)
	}
}

func TestEnsureAddsEveryAgent(t *testing.T) {
	f := newFake()
	p, err := NewPool(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Ensure(config.AgentConfig{Backend: "claude"})
	if len(f.added) != DefaultPoolSize || f.added["research-2"].Mode != Mode {
		t.Fatalf("added = %+v", f.added)
	}
	if got := strings.Join(p.Agents(), ","); got != "research-1,research-2" {
		t.Fatalf("Agents() = %s", got)
	}
}

func TestDispatchPicksLeastBusyAndStartsStopped(t *testing.T) {
	f := newFake("research-1", "research-2", "research-3")
	f.depth["research-1"] = 1
	f.inflight["research-2"] = "busy"
	f.states["research-3"].State = crux.StateStopped
	p, _ := NewPool(f, 3)

	id, err := p.Dispatch(context.Background(), "q1", map[string]string{"meeting_id": "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, kickIDPrefix) || f.queued[0] != "research-3:"+id {
		t.Fatalf("queued = %v id = %s, want research-3 (idle)", f.queued, id)
	}
	if len(f.started) != 1 || f.started[0] != "research-3" {
		t.Fatalf("started = %v, want the stopped agent started", f.started)
	}
	if !strings.Contains(f.prompts[0], "Question: q1") {
		t.Fatalf("prompt = %q", f.prompts[0])
	}

	// Ties go to the lowest-numbered agent; a paused agent is skipped.
	f.states["research-1"].Paused = true
	f.depth["research-3"] = 1 // now research-2 (1 in flight) ties research-3 (1 queued)
	if _, err := p.Dispatch(context.Background(), "q2", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.queued[1]; !strings.HasPrefix(got, "research-2:") {
		t.Fatalf("second dispatch went to %s, want research-2", got)
	}
}

func TestDispatchErrors(t *testing.T) {
	f := newFake("research-1")
	p, _ := NewPool(f, 1)
	ctx := context.Background()
	if _, err := p.Dispatch(ctx, "  ", nil); !errors.Is(err, ErrEmptyQuery) {
		t.Errorf("empty query err = %v", err)
	}

	f.queueErr = crux.ErrKickQueueFull
	if _, err := p.Dispatch(ctx, "q", nil); !errors.Is(err, crux.ErrKickQueueFull) {
		t.Errorf("queue err = %v", err)
	}
	if len(p.tracked) != 0 || len(p.order) != 0 {
		t.Errorf("a failed enqueue stayed tracked: %v", p.order)
	}
	f.queueErr = nil

	f.states["research-1"].State = crux.StateStopped
	f.startErr = errors.New("no tmux")
	if _, err := p.Dispatch(ctx, "q", nil); err == nil || !strings.Contains(err.Error(), "no tmux") {
		t.Errorf("start err = %v", err)
	}
	f.startErr = nil

	p.randRead = func([]byte) (int, error) { return 0, errors.New("entropy gone") }
	if _, err := p.Dispatch(ctx, "q", nil); err == nil || !strings.Contains(err.Error(), "entropy gone") {
		t.Errorf("id err = %v", err)
	}

	empty, _ := NewPool(newFake(), 1)
	if _, err := empty.Dispatch(ctx, "q", nil); !errors.Is(err, ErrNoAgents) {
		t.Errorf("no agents err = %v", err)
	}
}

func TestObserveTranslatesLifecycle(t *testing.T) {
	f := newFake("research-1")
	p, _ := NewPool(f, 1)
	var got []Event
	remove := p.Observe(func(ev Event) { got = append(got, ev) })

	id, err := p.Dispatch(context.Background(), "q", map[string]string{"rid": "r1"})
	if err != nil {
		t.Fatal(err)
	}
	emit := func(ev crux.KickEvent) {
		ev.Agent = "research-1"
		f.observer(ev)
	}
	emit(crux.KickEvent{Event: crux.KickEventDelivered, KickID: id})
	emit(crux.KickEvent{Event: crux.KickEventOutputSignal, KickID: id, Signal: crux.OutputSignalStatus, Text: "reading"})
	emit(crux.KickEvent{Event: crux.KickEventOutputSignal, KickID: id, Signal: crux.OutputSignalFinding, Text: "f1"})
	emit(crux.KickEvent{Event: crux.KickEventArchived, KickID: id})
	emit(crux.KickEvent{Event: crux.KickEventOutputSignal, KickID: "other", Signal: crux.OutputSignalFinding})
	emit(crux.KickEvent{Event: crux.KickEventDelivered})
	emit(crux.KickEvent{Event: crux.KickEventOutputSignal, KickID: id, Signal: crux.OutputSignalComplete, Text: "summary"})
	emit(crux.KickEvent{Event: crux.KickEventCompleted, KickID: id}) // after done: ignored

	kinds := make([]string, 0, len(got))
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
		if ev.KickID != id || ev.Agent != "research-1" || ev.Meta["rid"] != "r1" {
			t.Fatalf("event = %+v", ev)
		}
	}
	if strings.Join(kinds, ",") != "queued,running,finding,done" {
		t.Fatalf("kinds = %v", kinds)
	}
	if got[2].Text != "f1" || got[3].Text != "summary" {
		t.Fatalf("texts = %q %q", got[2].Text, got[3].Text)
	}

	// Turn end without [COMPLETE] is done; [ERROR] and kick-failed are failed.
	got = nil
	a, _ := p.Dispatch(context.Background(), "a", nil)
	b, _ := p.Dispatch(context.Background(), "b", nil)
	c, _ := p.Dispatch(context.Background(), "c", nil)
	emit(crux.KickEvent{Event: crux.KickEventCompleted, KickID: a, Detail: "ended"})
	emit(crux.KickEvent{Event: crux.KickEventOutputSignal, KickID: b, Signal: crux.OutputSignalError, Text: "blocked"})
	emit(crux.KickEvent{Event: crux.KickEventFailed, KickID: c, Detail: "timeout"})
	want := map[string]string{a: EventDone + ":", b: EventFailed + ":blocked", c: EventFailed + ":timeout"}
	for _, ev := range got {
		if ev.Kind == EventQueued {
			continue
		}
		if w := want[ev.KickID]; w != ev.Kind+":"+ev.Text {
			t.Errorf("kick %s: got %s:%s, want %s", ev.KickID, ev.Kind, ev.Text, w)
		}
		delete(want, ev.KickID)
	}
	if len(want) != 0 {
		t.Errorf("missing terminal events for %v", want)
	}

	remove()
	if f.observer != nil {
		t.Fatal("remover did not unregister")
	}
}

func TestTrackBoundsMemory(t *testing.T) {
	p, _ := NewPool(newFake("research-1"), 1)
	for i := 0; i < maxTrackedKicks+3; i++ {
		p.track(strings.Repeat("x", i+1), "research-1", nil)
	}
	if len(p.tracked) != maxTrackedKicks || len(p.order) != maxTrackedKicks {
		t.Fatalf("tracked %d / order %d, want %d", len(p.tracked), len(p.order), maxTrackedKicks)
	}
	if _, ok := p.tracked["x"]; ok {
		t.Fatal("oldest tracked kick not evicted")
	}
}
