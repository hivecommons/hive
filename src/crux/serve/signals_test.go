package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/signals"
)

// nextEvent reads ch until an event of type typ arrives.
func nextEvent(t *testing.T, ch chan api.Event, typ string) api.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event stream closed before %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

// holdingLauncher reports each launch on launched and holds the command
// until release is closed or the run is cancelled.
func holdingLauncher(launched chan<- struct{}, release <-chan struct{}) runsLauncher {
	return func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		launched <- struct{}{}
		select {
		case <-release:
			return sandbox.Result{Stdout: "ok"}, nil
		case <-ctx.Done():
			return sandbox.Result{ExitCode: -1}, ctx.Err()
		}
	}
}

func TestRunSignalsRoutesAnswerNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.RunSignalsPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodPost, api.RunSignalsPath("run_x"), api.RunSignalRequest{Kind: api.SignalThinking}, http.StatusNotImplemented, api.CodeNotSupported)

	// A run service without a signal book reads empty pages and refuses
	// emits as unsupported.
	f, ws := newRunsServer(t, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) { return sandbox.Result{}, nil })
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &run)
	var page api.RunSignalPage
	f.expect(http.MethodGet, api.RunSignalsPath(run.ID), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Signals) != 0 {
		t.Fatalf("page = %+v", page)
	}
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.RunSignalRequest{Kind: api.SignalThinking}, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunSignalsFollowATurnAndArePublished(t *testing.T) {
	launched, release := make(chan struct{}, 1), make(chan struct{})
	f, ws := newRunsServer(t, holdingLauncher(launched, release))
	f.srv.Runs.Signals = signals.NewBook(signals.Policy{})
	// Rebuild the routes after wiring, as crux serve builds them once its
	// services are set; that is also what installs the event relay.
	f.ts.Config.Handler = f.srv.Handler()
	events := f.srv.subscribe()
	defer f.srv.unsubscribe(events)

	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"agent-step"}, Detach: true}, http.StatusAccepted, &run)
	<-launched
	ev := nextEvent(t, events, api.EventRunSignal)
	var relayed api.RunSignalEvent
	if err := json.Unmarshal(ev.Data, &relayed); err != nil || relayed.Run != run.ID || relayed.Kind != api.SignalStarted || relayed.Source != api.SignalSourceRunner || relayed.Seq != 1 {
		t.Fatalf("started event = %+v, %v", relayed, err)
	}

	// The owner reports where the turn is; the source is always caller.
	var tool api.RunSignal
	f.expect(http.MethodPost, api.RunSignalsPath(run.ID), api.RunSignalRequest{Kind: api.SignalTool, Message: "running go test", Data: json.RawMessage(`{"tool":"bash"}`)}, http.StatusCreated, &tool)
	if tool.Seq != 2 || tool.Source != api.SignalSourceCaller || tool.Message != "running go test" || string(tool.Data) != `{"tool":"bash"}` {
		t.Fatalf("tool = %+v", tool)
	}
	if ev := nextEvent(t, events, api.EventRunSignal); !strings.Contains(string(ev.Data), `"kind":"tool"`) {
		t.Fatalf("tool event = %s", ev.Data)
	}
	// Boundaries stay the runner's; the kind set is closed; bodies are strict.
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.RunSignalRequest{Kind: api.SignalDone}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.RunSignalRequest{Kind: "sleeping"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), `{"kind":"tool","data":{"a":}}`, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), `{"kind":"tool","source":"runner"}`, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath("run_000000000000"), api.RunSignalRequest{Kind: api.SignalTool}, http.StatusNotFound, api.CodeNotFound)

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.srv.Runs.Wait(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	var page api.RunSignalPage
	f.expect(http.MethodGet, api.RunSignalsPath(run.ID), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Signals) != 3 || page.NextSeq != 3 || page.More {
		t.Fatalf("page = %+v", page)
	}
	done := page.Signals[2]
	var data api.SignalDoneData
	if done.Kind != api.SignalDone || json.Unmarshal(done.Data, &data) != nil || data.Status != api.RunCompleted || data.ExitCode == nil || *data.ExitCode != 0 {
		t.Fatalf("done = %+v %+v", done, data)
	}
	// A follower resumes after what it saw; paging is validated.
	f.expect(http.MethodGet, api.RunSignalsQuery(run.ID, 2, 5), nil, http.StatusOK, &page)
	if len(page.Signals) != 1 || page.Signals[0].Seq != 3 {
		t.Fatalf("resumed page = %+v", page)
	}
	f.expectCode(http.MethodGet, api.RunSignalsPath(run.ID)+"?since_seq=-1", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunSignalsPath(run.ID)+"?limit=0", nil, http.StatusBadRequest, api.CodeBadRequest)
	// Nothing follows done.
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.RunSignalRequest{Kind: api.SignalThinking}, http.StatusConflict, api.CodeSignalsClosed)
}

func TestRunSignalsReportACorruptLog(t *testing.T) {
	f, ws := newRunsServer(t, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) { return sandbox.Result{}, nil })
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &run)
	if err := os.WriteFile(filepath.Join(f.srv.Runs.Store.Path(run.ID), signals.FileName), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.expectCode(http.MethodGet, api.RunSignalsPath(run.ID), nil, http.StatusInternalServerError, api.CodeRuntime)
}

// withRuns gives the gate fixture a run service with turn signals whose
// launcher holds every command until the test ends, then rebuilds the
// routes the way crux serve wires both services before serving.
func (f *gateFixture) withRuns() string {
	f.t.Helper()
	store, err := runstate.Open(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	root := f.t.TempDir()
	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		f.t.Fatal(err)
	}
	launched, release := make(chan struct{}, 8), make(chan struct{})
	f.t.Cleanup(func() { close(release) })
	backend := &runs.Backend{Name: "fake", Launcher: holdingLauncher(launched, release), Capabilities: runs.Capabilities{NetworkNone: true}}
	svc := runs.New(store, backend, runs.Config{Image: "img", WorkspaceRoots: []string{root}})
	svc.Signals = signals.NewBook(signals.Policy{Now: f.clock.now})
	f.s.Runs = svc
	f.h = f.s.Handler()
	return ws
}

// runSignals is the gate-sourced signals of run id, read as who.
func (f *gateFixture) runSignals(who, id string) []api.RunSignal {
	f.t.Helper()
	var page api.RunSignalPage
	if code, e := f.call(who, http.MethodGet, api.RunSignalsPath(id), nil, &page); code != http.StatusOK {
		f.t.Fatalf("signals: %d %+v", code, e)
	}
	// started is the runner's and races the gate; the gate's own signals
	// are what this fixture checks.
	var out []api.RunSignal
	for _, sig := range page.Signals {
		if sig.Source == api.SignalSourceGate {
			out = append(out, sig)
		}
	}
	return out
}

func TestGateOpenedForARunSignalsItsTurn(t *testing.T) {
	f := newGateFixture(t)
	ws := f.withRuns()
	var run api.ContainedRun
	if code, e := f.call("atlas", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"step"}, Detach: true}, &run); code != http.StatusAccepted {
		t.Fatalf("run: %d %+v", code, e)
	}

	// The run must exist and be the opener's.
	if code, e := f.call("beacon", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: run.ID}, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon on atlas's run: %d %+v", code, e)
	}
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: "run_000000000000"}, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("unknown run: %d %+v", code, e)
	}

	req := api.GateRequest{Subject: "Spend $40 on cache-17?", Choices: []string{"yes", "no"}, Run: " " + run.ID + " "}
	var g api.Gate
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, req, &g); code != http.StatusCreated || g.Run != run.ID {
		t.Fatalf("open: %d %+v %+v", code, e, g)
	}
	// Re-opening is idempotent and signals nothing new; the same subject
	// without the run is a different gate.
	var again, unbound api.Gate
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, req, &again); code != http.StatusOK || again.ID != g.ID {
		t.Fatalf("reopen: %d %+v", code, again)
	}
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: req.Subject, Choices: req.Choices}, &unbound); code != http.StatusCreated || unbound.ID == g.ID || unbound.Run != "" {
		t.Fatalf("unbound: %d %+v", code, unbound)
	}
	sigs := f.runSignals("atlas", run.ID)
	var data api.SignalGateData
	if len(sigs) != 1 || sigs[0].Kind != api.SignalNeedsInput || sigs[0].Message != req.Subject || json.Unmarshal(sigs[0].Data, &data) != nil || data.Gate != g.ID {
		t.Fatalf("after open: %+v", sigs)
	}

	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "yes", Note: "fine"}, nil); code != http.StatusOK {
		t.Fatalf("answer: %d %+v", code, e)
	}
	sigs = f.runSignals("atlas", run.ID)
	data = api.SignalGateData{}
	if len(sigs) != 2 || sigs[1].Kind != api.SignalInput || json.Unmarshal(sigs[1].Data, &data) != nil || data.Gate != g.ID || data.Answer != "yes" || data.AnsweredBy != "kelly" || strings.Contains(string(sigs[1].Data), "fine") {
		t.Fatalf("after answer: %+v %+v", sigs, data)
	}

	// Expiry is blocked, neither approval nor denial.
	var timed api.Gate
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "Keep going?", Timeout: "1m", Run: run.ID}, &timed); code != http.StatusCreated {
		t.Fatalf("open timed: %d %+v", code, e)
	}
	f.clock.advance(2 * time.Minute)
	if code, _ := f.call("atlas", http.MethodGet, api.GatePath(timed.ID), nil, &timed); code != http.StatusOK || timed.Status != api.GateExpired {
		t.Fatalf("expired: %d %+v", code, timed)
	}
	sigs = f.runSignals("atlas", run.ID)
	if len(sigs) != 4 || sigs[2].Kind != api.SignalNeedsInput || sigs[3].Kind != api.SignalBlocked {
		t.Fatalf("after expiry: %+v", sigs)
	}
	// Settle the run before the temp dirs go, so its goroutine is not
	// writing while they are removed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.s.Runs.Cancel(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGateRunNeedsARunService(t *testing.T) {
	f := newGateFixture(t)
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: "run_x"}, nil); code != http.StatusNotImplemented || e.Code != api.CodeNotSupported {
		t.Fatalf("gate on a run without runs: %d %+v", code, e)
	}
}

func TestKickOutcomeIsSavedBeforeItIsPublishedAndSettlesAgainstTheLastKick(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusNotImplemented, api.CodeNotSupported)

	dir := t.TempDir()
	store, err := signals.OpenAgentOutcomes(dir, signals.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.KickOutcomes = store
	kick := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p := f.rt.Add("alpha", "a1", crux.StateRunning)
	p.LastKick = &kick

	// Kicked, not yet ended: no outcome, not settled.
	var got api.AgentKickOutcome
	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &got)
	if got.Outcome != nil || got.SettledForLastKick || got.LastKick == nil || !got.LastKick.Equal(kick) {
		t.Fatalf("before the turn ends: %+v", got)
	}

	events := f.srv.subscribe()
	defer f.srv.unsubscribe(events)
	f.srv.recordKickOutcome("alpha", crux.KickOutcome{Kind: crux.KickOutcomeNoOp, Reason: "no PR opened", At: kick.Add(time.Minute), KickAt: kick})
	ev := nextEvent(t, events, api.EventKickOutcome)
	if ev.Agent != "alpha" {
		t.Fatalf("event = %+v", ev)
	}
	// The record is already there when the event is.
	if saved, err := store.Load("alpha"); err != nil || saved.Outcome.Kind != crux.KickOutcomeNoOp {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &got)
	if got.Outcome == nil || got.Outcome.Kind != crux.KickOutcomeNoOp || got.Outcome.Reason != "no PR opened" || !got.SettledForLastKick {
		t.Fatalf("settled: %+v", got)
	}

	// A newer kick: the saved outcome describes the earlier turn.
	next := kick.Add(time.Hour)
	p.LastKick = &next
	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &got)
	if got.Outcome == nil || got.SettledForLastKick || !got.Outcome.KickAt.Equal(kick) {
		t.Fatalf("after a newer kick: %+v", got)
	}

	// The runtime no longer holds the agent (crux serve restarted, the
	// definition was removed): the record still answers, unsettled.
	f.srv.recordKickOutcome("gone", crux.KickOutcome{Kind: crux.KickOutcomeEnded, At: kick, KickAt: kick})
	// last_kick is omitempty: decode into a fresh value so alpha's does not
	// survive into gone's answer.
	got = api.AgentKickOutcome{}
	f.expect(http.MethodGet, api.AgentKickOutcomePath("gone"), nil, http.StatusOK, &got)
	if got.Outcome == nil || got.LastKick != nil || got.SettledForLastKick {
		t.Fatalf("unknown to the runtime: %+v", got)
	}
	f.expectCode(http.MethodGet, api.AgentKickOutcomePath("ghost"), nil, http.StatusNotFound, api.CodeNotFound)

	// A corrupt record is a server error, not a missing one.
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.expectCode(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusInternalServerError, api.CodeRuntime)
}
