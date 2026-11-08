package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/signals"
)

func TestRunsSignalsRoutesAnswerNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.RunSignalsPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodPost, api.RunSignalsPath("run_x"), api.SignalRequest{Kind: api.SignalTool}, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunsSignalsListAndEmit(t *testing.T) {
	f, ws := newRunsServer(t, func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "ran " + spec.Command[0]}, nil
	})
	f.srv.Runs.SignalLog = signals.FileOpener(signals.Policy{})
	// The fixture built its Handler before Runs existed; wire the observer
	// the way registerRuns does so events carry each durable signal.
	f.srv.Runs.SignalObserver = f.srv.publishSignal
	events := f.srv.subscribe()
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"make", "test"}}, http.StatusOK, &run)

	var page api.SignalPage
	f.expect(http.MethodGet, api.RunSignalsPath(run.ID), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Signals) != 2 || page.NextSeq != 2 || page.More {
		t.Fatalf("page = %+v", page)
	}
	if page.Signals[0].Kind != api.SignalStarted || page.Signals[1].Kind != api.SignalDone || page.Signals[1].Source != api.SignalSourceRunner {
		t.Fatalf("runner signals = %+v", page.Signals)
	}

	// The owner emits a caller signal; it is recorded as the principal.
	var rec api.Signal
	f.expect(http.MethodPost, api.RunSignalsPath(run.ID), api.SignalRequest{Kind: api.SignalBlocked, Message: "waiting on ghp_abcdefghijklmnopqrstuvwxyz0123456789", Data: json.RawMessage(`{"k":1}`)}, http.StatusCreated, &rec)
	if rec.Seq != 3 || rec.Kind != api.SignalBlocked || rec.Source != api.SignalSourceCaller || rec.By != "operator:"+DefaultOperatorName || strings.Contains(rec.Message, "ghp_abc") || string(rec.Data) != `{"k":1}` {
		t.Fatalf("emitted = %+v", rec)
	}
	// A follower resumes after what it saw.
	var next api.SignalPage
	f.expect(http.MethodGet, api.RunSignalsQuery(run.ID, 2, 10), nil, http.StatusOK, &next)
	if len(next.Signals) != 1 || next.Signals[0].Seq != 3 || next.NextSeq != 3 {
		t.Fatalf("resumed = %+v", next)
	}

	// Refusals: started/done are the runner's, unknown kinds are 400,
	// oversize data is 413, paging is validated, unknown runs 404.
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.SignalRequest{Kind: api.SignalDone}, http.StatusForbidden, api.CodeForbidden)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.SignalRequest{Kind: "finished"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.SignalRequest{Kind: api.SignalTool, Data: json.RawMessage(`"` + strings.Repeat("d", signals.MaxDataBytes) + `"`)}, http.StatusRequestEntityTooLarge, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), "{", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunSignalsPath(run.ID)+"?since_seq=-1", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunSignalsPath(run.ID)+"?limit=0", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunSignalsPath("run_000000000000"), nil, http.StatusNotFound, api.CodeNotFound)
	f.expectCode(http.MethodPost, api.RunSignalsPath("run_000000000000"), api.SignalRequest{Kind: api.SignalTool}, http.StatusNotFound, api.CodeNotFound)

	// Three durable signals, three run.signal events, in order.
	var got []api.RunSignalEvent
	for len(got) < 3 {
		select {
		case ev := <-events:
			if ev.Type != api.EventRunSignal {
				continue
			}
			var rs api.RunSignalEvent
			if err := json.Unmarshal(ev.Data, &rs); err != nil {
				t.Fatal(err)
			}
			got = append(got, rs)
		case <-time.After(10 * time.Second):
			t.Fatalf("events: got %d of 3", len(got))
		}
	}
	if got[0].Run != run.ID || got[0].Signal.Kind != api.SignalStarted || got[2].Signal.Kind != api.SignalBlocked {
		t.Fatalf("events = %+v", got)
	}
}

func TestRunsSignalsAreEmptyWhenNoLogIsKept(t *testing.T) {
	f, ws := newRunsServer(t, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	})
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &run)
	var page api.SignalPage
	f.expect(http.MethodGet, api.RunSignalsQuery(run.ID, 4, 0), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Signals) != 0 || page.NextSeq != 4 {
		t.Fatalf("page = %+v", page)
	}
	f.expectCode(http.MethodPost, api.RunSignalsPath(run.ID), api.SignalRequest{Kind: api.SignalTool}, http.StatusNotImplemented, api.CodeNotSupported)
}

// withRuns gives the gate fixture a runs service (no backend: records
// only) with a signal log, and a run owned by owner.
func (f *gateFixture) withRuns(owner string) string {
	f.t.Helper()
	store, err := runstate.Open(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	svc := runs.New(store, nil, runs.Config{})
	svc.SignalLog = signals.FileOpener(signals.Policy{})
	rec, _, err := store.Create(runstate.Record{Kind: runstate.KindRun, Owner: owner})
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.Runs = svc
	f.h = f.s.Handler()
	return rec.ID
}

func TestGateOnARunEmitsNeedsInputThenInput(t *testing.T) {
	f := newGateFixture(t)
	runID := f.withRuns("atlas")
	var g api.Gate
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "spend $40?", Run: runID}, &g); code != http.StatusCreated || g.Run != runID {
		t.Fatalf("open: %d %+v %+v", code, e, g)
	}
	// Idempotent re-open writes no second signal.
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "spend $40?", Run: runID}, nil); code != http.StatusOK {
		t.Fatalf("reopen: %d", code)
	}
	// The same subject without the run is a different gate.
	var other api.Gate
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "spend $40?"}, &other); code != http.StatusCreated || other.ID == g.ID {
		t.Fatalf("open without run: %d %+v", code, other)
	}
	if code, e := f.call("operator", http.MethodPost, api.GateAnswerPath(g.ID), api.GateAnswerRequest{Answer: "approve", Note: "go"}, nil); code != http.StatusOK {
		t.Fatalf("answer: %d %+v", code, e)
	}
	var page api.SignalPage
	if code, e := f.call("atlas", http.MethodGet, api.RunSignalsPath(runID), nil, &page); code != http.StatusOK {
		t.Fatalf("signals: %d %+v", code, e)
	}
	if len(page.Signals) != 2 {
		t.Fatalf("signals = %+v", page.Signals)
	}
	need, in := page.Signals[0], page.Signals[1]
	var needData, inData map[string]any
	if need.Kind != api.SignalNeedsInput || need.Source != api.SignalSourceGate || need.Message != "spend $40?" || json.Unmarshal(need.Data, &needData) != nil || needData["gate"] != g.ID || needData["status"] != api.GateWaiting {
		t.Fatalf("needs-input = %+v %s", need, need.Data)
	}
	if in.Kind != api.SignalInput || in.Message != "approve" || json.Unmarshal(in.Data, &inData) != nil || inData["answer"] != "approve" || inData["answered_by"] != "kelly" {
		t.Fatalf("input = %+v %s", in, in.Data)
	}
}

func TestGateOnARunExpiryEmitsBlocked(t *testing.T) {
	f := newGateFixture(t)
	runID := f.withRuns("atlas")
	var g api.Gate
	if code, _ := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "quick", Timeout: "1m", Run: runID}, &g); code != http.StatusCreated {
		t.Fatalf("open: %d", code)
	}
	f.clock.advance(2 * time.Minute)
	if code, _ := f.call("atlas", http.MethodGet, api.GatePath(g.ID), nil, nil); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	var page api.SignalPage
	f.call("operator", http.MethodGet, api.RunSignalsPath(runID), nil, &page)
	if len(page.Signals) != 2 || page.Signals[1].Kind != api.SignalBlocked || page.Signals[1].Source != api.SignalSourceGate {
		t.Fatalf("signals = %+v", page.Signals)
	}
}

func TestGateRunIsCheckedBeforeTheGateOpens(t *testing.T) {
	f := newGateFixture(t)
	// No runs service: 501.
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: "run_x"}, nil); code != http.StatusNotImplemented || e.Code != api.CodeNotSupported {
		t.Fatalf("no runs: %d %+v", code, e)
	}
	runID := f.withRuns("atlas")
	if code, e := f.call("atlas", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: "run_000000000000"}, nil); code != http.StatusNotFound || e.Code != api.CodeNotFound {
		t.Fatalf("unknown run: %d %+v", code, e)
	}
	// beacon does not own atlas's run; the operator may attach to any.
	if code, e := f.call("beacon", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: runID}, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("foreign run: %d %+v", code, e)
	}
	if code, _ := f.call("operator", http.MethodPost, api.PathGates, api.GateRequest{Subject: "s", Run: runID}, nil); code != http.StatusCreated {
		t.Fatalf("operator attach: %d", code)
	}
	var list api.GateList
	f.call("atlas", http.MethodGet, api.PathGates, nil, &list)
	if len(list.Gates) != 0 {
		t.Fatalf("refused opens left gates: %+v", list.Gates)
	}
}

func TestAgentKickOutcomeRoute(t *testing.T) {
	f := newFamServer(t)
	kickAt := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p := f.rt.Add("alpha", "a1", crux.StateRunning)
	p.LastKick = &kickAt
	store, err := signals.OpenOutcomes(signals.DefaultDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.KickOutcomes = store

	// Kicked, nothing classified yet, nothing saved: unsettled, no outcome.
	var out api.AgentKickOutcome
	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &out)
	if out.Agent != "alpha" || out.Outcome != nil || out.SettledForLastKick || out.LastKick == nil || !out.LastKick.Equal(kickAt) {
		t.Fatalf("fresh = %+v", out)
	}

	// The saved record of an earlier turn answers but does not settle this kick.
	earlier := api.KickOutcome{Kind: crux.KickOutcomeEnded, At: kickAt.Add(-time.Hour), KickAt: kickAt.Add(-2 * time.Hour)}
	if err := store.Put("alpha", earlier); err != nil {
		t.Fatal(err)
	}
	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &out)
	if out.Outcome == nil || out.Outcome.Kind != crux.KickOutcomeEnded || !out.Saved || out.SettledForLastKick {
		t.Fatalf("saved earlier = %+v", out)
	}

	// The live runtime classified this kick's turn: settled, live wins.
	p.KickOutcome = crux.KickOutcome{Kind: crux.KickOutcomeNoOp, Reason: "no PR opened", At: kickAt.Add(time.Minute), KickAt: kickAt}
	f.expect(http.MethodGet, api.AgentKickOutcomePath("alpha"), nil, http.StatusOK, &out)
	if out.Outcome == nil || out.Outcome.Kind != crux.KickOutcomeNoOp || out.Outcome.Reason != "no PR opened" || out.Saved || !out.SettledForLastKick {
		t.Fatalf("live = %+v", out)
	}
	f.expectCode(http.MethodGet, api.AgentKickOutcomePath("nope"), nil, http.StatusNotFound, api.CodeNotFound)
}

func TestKickOutcomeObserverPersistsEachSettle(t *testing.T) {
	rt := cruxtest.NewRecorder()
	rt.Add("alpha", "a1", crux.StateRunning)
	srv := New(rt)
	store, err := signals.OpenOutcomes(signals.DefaultDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	srv.KickOutcomes = store
	uninstall := srv.installHostObservers()
	defer uninstall()
	ko, _, _, _, _, _, _, _ := rt.Observers()
	kickAt := time.Unix(100, 0).UTC()
	ko("alpha", crux.KickOutcome{Kind: crux.KickOutcomeStandDown, Reason: "policy", At: kickAt.Add(time.Second), KickAt: kickAt})
	saved, ok, err := store.Get("alpha")
	if err != nil || !ok || saved.Kind != crux.KickOutcomeStandDown || !saved.KickAt.Equal(kickAt) {
		t.Fatalf("saved = %+v %v %v", saved, ok, err)
	}
	// A bad agent name is logged, not fatal; nothing saved.
	ko("../etc", crux.KickOutcome{Kind: crux.KickOutcomeEnded, KickAt: kickAt})
	if _, _, err := store.Get("../etc"); err == nil {
		t.Fatal("bad name reached the store")
	}
	// Without a store the observer still publishes.
	srv.KickOutcomes = nil
	ko("alpha", crux.KickOutcome{Kind: crux.KickOutcomeEnded, KickAt: kickAt})
}
