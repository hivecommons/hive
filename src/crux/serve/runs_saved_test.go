package serve

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// newSavedRunsFixture is a token-bearing server with two lane projects and
// a run service over a fake launcher that echoes its command; the call
// helper is the checkout fixture's.
func newSavedRunsFixture(t *testing.T) (*checkoutFixture, string) {
	t.Helper()
	tokens := map[string]string{"atlas": strings.Repeat("a", 32), "beacon": strings.Repeat("b", 32), "operator": "agent-admin"}
	cfg := lanes.Config{Principals: []lanes.Principal{{Project: "atlas", TokenEnv: "ATLAS"}, {Project: "beacon", TokenEnv: "BEACON"}}}
	auth, err := lanes.Bind(cfg, func(env string) string { return tokens[strings.ToLower(env)] }, tokens["operator"])
	if err != nil {
		t.Fatal(err)
	}
	store, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := runsLauncher(func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: strings.Join(spec.Command, " ")}, nil
	})
	f := &checkoutFixture{t: t, inUse: map[string]bool{}, tokens: tokens}
	f.s = New(newFakeRuntime())
	f.s.Token, f.s.LaneAuth = tokens["operator"], auth
	f.s.Runs = runs.New(store, &runs.Backend{Name: "fake", Launcher: launcher, Capabilities: runs.Capabilities{NetworkNone: true}}, runs.Config{Image: "img", WorkspaceRoots: []string{root}})
	f.s.LaunchContext = context.Background()
	f.h = f.s.Handler()
	return f, ws
}

func TestRunsSavedRoutesAnswerNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.PathRuns, nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodPost, api.RunRestartPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodGet, api.RunHistoryPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunsListIsScopedToTheCallerUnlessOperator(t *testing.T) {
	f, ws := newSavedRunsFixture(t)
	var a, b api.ContainedRun
	if code, e := f.call("atlas", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"a"}}, &a); code != http.StatusOK {
		t.Fatalf("atlas create: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"b"}}, &b); code != http.StatusOK {
		t.Fatalf("beacon create: %d %+v", code, e)
	}
	var mine api.ContainedRunList
	if code, e := f.call("atlas", http.MethodGet, api.PathRuns, nil, &mine); code != http.StatusOK || len(mine.Runs) != 1 || mine.Runs[0].ID != a.ID || mine.Runs[0].Stdout != "" {
		t.Fatalf("atlas list: %d %+v %+v", code, e, mine)
	}
	var all api.ContainedRunList
	if code, _ := f.call("operator", http.MethodGet, api.PathRuns, nil, &all); code != http.StatusOK || len(all.Runs) != 2 || all.Runs[0].ID != b.ID {
		t.Fatalf("operator list: %d %+v", code, all)
	}
	var none api.ContainedRunList
	if code, _ := f.call("operator", http.MethodGet, api.RunsListQuery(api.RunUnfinished, 0), nil, &none); code != http.StatusOK || len(none.Runs) != 0 {
		t.Fatalf("status filter: %d %+v", code, none)
	}
	var one api.ContainedRunList
	if code, _ := f.call("operator", http.MethodGet, api.RunsListQuery("", 1), nil, &one); code != http.StatusOK || len(one.Runs) != 1 {
		t.Fatalf("limit: %d %+v", code, one)
	}
	if code, e := f.call("operator", http.MethodGet, api.PathRuns+"?limit=0", nil, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("limit=0: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodGet, api.PathRuns+"?status=done", nil, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("bad status: %d %+v", code, e)
	}
}

func TestRunsRestartIsOwnedAuditedAndRefusedWhileLive(t *testing.T) {
	f, ws := newSavedRunsFixture(t)
	events := f.s.subscribe()
	defer f.s.unsubscribe(events)
	var first api.ContainedRun
	if code, e := f.call("atlas", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"make", "test"}}, &first); code != http.StatusOK {
		t.Fatalf("create: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodPost, api.RunRestartPath(first.ID), api.RunRestartRequest{}, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon restart: %d %+v", code, e)
	}
	if code, e := f.call("operator", http.MethodPost, api.RunRestartPath("run_missing"), api.RunRestartRequest{}, nil); code != http.StatusNotFound || e.Code != api.CodeNotFound {
		t.Fatalf("missing restart: %d %+v", code, e)
	}
	var again api.ContainedRun
	if code, e := f.call("operator", http.MethodPost, api.RunRestartPath(first.ID), api.RunRestartRequest{Reason: "flaky", IdempotencyKey: "r1"}, &again); code != http.StatusOK {
		t.Fatalf("restart: %d %+v", code, e)
	}
	if again.ID == first.ID || again.RestartedFrom != first.ID || again.Owner != "atlas" || again.Status != api.RunCompleted || again.Stdout != "make test" {
		t.Fatalf("restarted = %+v", again)
	}
	var replay api.ContainedRun
	if code, _ := f.call("operator", http.MethodPost, api.RunRestartPath(first.ID), api.RunRestartRequest{IdempotencyKey: "r1"}, &replay); code != http.StatusOK || replay.ID != again.ID {
		t.Fatalf("replay: %d %+v", code, replay)
	}
	var old api.ContainedRun
	if code, _ := f.call("atlas", http.MethodGet, api.RunPath(first.ID), nil, &old); code != http.StatusOK || old.RestartedBy != again.ID {
		t.Fatalf("old record: %d %+v", code, old)
	}
	var hist api.RunHistory
	if code, _ := f.call("atlas", http.MethodGet, api.RunHistoryPath(again.ID), nil, &hist); code != http.StatusOK || hist.Run != again.ID || len(hist.Transitions) != 3 || hist.Transitions[0].Status != api.RunAccepted || hist.Transitions[2].Status != api.RunCompleted {
		t.Fatalf("history: %d %+v", code, hist)
	}
	if code, e := f.call("beacon", http.MethodGet, api.RunHistoryPath(again.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon history: %d %+v", code, e)
	}

	var audits []string
	deadline := time.After(10 * time.Second)
	for len(audits) == 0 {
		select {
		case ev := <-events:
			if ev.Type == api.EventAudit {
				audits = append(audits, string(ev.Data))
			}
		case <-deadline:
			t.Fatal("no audit event for the restart")
		}
	}
	if a := audits[0]; !strings.Contains(a, auditRunRestart) || !strings.Contains(a, first.ID) || !strings.Contains(a, again.ID) || !strings.Contains(a, "flaky") {
		t.Fatalf("audit = %s", a)
	}

	// A record whose saved state is still live refuses the restart.
	rec, _, err := f.s.Runs.Store.Create(runstate.Record{Owner: "atlas", Detail: []byte(`{"request":{"command":["x"]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Runs.Store.Update(rec.ID, func(r *runstate.Record) error { r.Status = runstate.StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, e := f.call("atlas", http.MethodPost, api.RunRestartPath(rec.ID), api.RunRestartRequest{}, nil); code != http.StatusConflict || e.Code != api.CodeConflict {
		t.Fatalf("live restart: %d %+v", code, e)
	}
	// One that predates the saved request cannot be re-run either.
	legacy, _, _ := f.s.Runs.Store.Create(runstate.Record{Owner: "atlas"})
	f.s.Runs.Store.Update(legacy.ID, func(r *runstate.Record) error { r.Status = runstate.StatusUnfinished; return nil })
	if code, e := f.call("atlas", http.MethodPost, api.RunRestartPath(legacy.ID), api.RunRestartRequest{}, nil); code != http.StatusConflict || e.Code != api.CodeConflict {
		t.Fatalf("legacy restart: %d %+v", code, e)
	}
	if code, e := f.call("atlas", http.MethodPost, api.RunRestartPath(legacy.ID), map[string]any{"bogus": 1}, nil); code != http.StatusBadRequest || e.Code != api.CodeBadRequest {
		t.Fatalf("bad body: %d %+v", code, e)
	}
}

func TestRunsRestartDetachedReturnsAccepted(t *testing.T) {
	f, ws := newRunsServer(t, func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	})
	f.srv.LaunchContext = context.Background()
	var first api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &first)
	var again api.ContainedRun
	f.expect(http.MethodPost, api.RunRestartPath(first.ID), api.RunRestartRequest{Detach: true}, http.StatusAccepted, &again)
	if again.Status != api.RunAccepted || again.RestartedFrom != first.ID {
		t.Fatalf("detached restart = %+v", again)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if done, err := f.srv.Runs.Wait(ctx, again.ID); err != nil || done.Status != api.RunCompleted {
		t.Fatalf("wait = %+v, %v", done, err)
	}
}
