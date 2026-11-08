package runs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentstep"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestRestartReRunsTheSavedRequestAndLinksBothRecords(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: strings.Join(spec.Command, " ")}, nil
	}))
	req := api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"make", "test"}, EnvAllowlist: []string{"KEEP"}, TimeoutMs: 5000, IdempotencyKey: "orig", Detach: true}
	first := f.wait(f.start(req).ID)

	again, created, err := f.svc.Restart(context.Background(), operator, first.ID, api.RunRestartRequest{Reason: "flaky", IdempotencyKey: "r1"})
	if err != nil || !created || again.ID == first.ID || again.RestartedFrom != first.ID || again.Status != api.RunAccepted {
		t.Fatalf("Restart = %+v created=%v err=%v", again, created, err)
	}
	done := f.wait(again.ID)
	if done.Stdout != "make test" || done.Owner != first.Owner || done.RestartedFrom != first.ID {
		t.Fatalf("restarted run = %+v", done)
	}
	f.spec() // the original launch
	if spec := f.spec(); spec.Env["KEEP"] != "yes" || strings.Join(spec.Command, " ") != "make test" {
		t.Fatalf("restart launched %+v", spec)
	}
	if old, _ := f.svc.Get(first.ID); old.RestartedBy != again.ID || old.Status != api.RunCompleted {
		t.Fatalf("old record = %+v", old)
	}
	// The old record's idempotency key still names the old run; the
	// restart's own key names the restart.
	if replay, created, _ := f.svc.Create(context.Background(), operator, req); created || replay.ID != first.ID {
		t.Fatalf("original key replay = %+v created=%v", replay, created)
	}
	if third, created, err := f.svc.Restart(context.Background(), operator, first.ID, api.RunRestartRequest{IdempotencyKey: "r1"}); err != nil || created || third.ID != again.ID {
		t.Fatalf("restart replay = %+v created=%v err=%v", third, created, err)
	}
	if len(f.launcher.specs) != 0 {
		t.Fatalf("replay launched again")
	}
	hist, err := f.svc.History(again.ID)
	if err != nil || hist.Run != again.ID || len(hist.Transitions) != 3 || hist.Transitions[2].Status != api.RunCompleted {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	if _, err := f.svc.History("run_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing history = %v", err)
	}
}

func TestRestartRefusesALiveRunAndRecordsWithoutARequest(t *testing.T) {
	f := newFixture(t, blocking())
	live := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep"}})
	f.spec()
	if _, _, err := f.svc.Restart(context.Background(), operator, live.ID, api.RunRestartRequest{}); !errors.Is(err, ErrLive) {
		t.Fatalf("restart of a live run = %v", err)
	}
	cancelled, err := f.svc.Cancel(context.Background(), live.ID)
	if err != nil || cancelled.Status != api.RunCancelled {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	again, created, err := f.svc.Restart(context.Background(), operator, live.ID, api.RunRestartRequest{Detach: true})
	if err != nil || !created {
		t.Fatalf("restart after cancel = created=%v err=%v", created, err)
	}
	f.spec()
	if _, err := f.svc.Cancel(context.Background(), again.ID); err != nil {
		t.Fatal(err)
	}

	legacy, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator", Detail: []byte(`{"command":["x"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Store.Update(legacy.ID, func(r *runstate.Record) error { r.Status = runstate.StatusUnfinished; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Restart(context.Background(), operator, legacy.ID, api.RunRestartRequest{}); !errors.Is(err, ErrNoRequest) {
		t.Fatalf("restart of a legacy record = %v", err)
	}
	if _, _, err := f.svc.Restart(context.Background(), operator, "run_missing", api.RunRestartRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restart of a missing run = %v", err)
	}
	unconfigured := New(f.svc.Store, nil, Config{})
	if _, _, err := unconfigured.Restart(context.Background(), operator, legacy.ID, api.RunRestartRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("restart without a backend = %v", err)
	}
}

func TestRestartRevalidatesAgainstTheCurrentConfiguration(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	gone := filepath.Join(f.root, "gone")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: gone, Command: []string{"x"}}).ID)
	f.spec()
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	var reqErr *RequestError
	if _, _, err := f.svc.Restart(context.Background(), operator, run.ID, api.RunRestartRequest{}); !errors.As(err, &reqErr) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("restart into a removed workspace = %v", err)
	}
	// The checkout owner, not the restarting operator, is who the request
	// is validated as.
	f.svc.Checkouts = fakeCheckouts{}
	project := Caller{Name: "project:atlas"}
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: project.Name, Detail: []byte(`{"request":{"checkout":"co_1","command":["x"]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Store.Update(rec.ID, func(r *runstate.Record) error { r.Status = runstate.StatusUnfinished; return nil })
	if _, _, err := f.svc.Restart(context.Background(), operator, rec.ID, api.RunRestartRequest{}); !errors.As(err, &reqErr) || !strings.Contains(err.Error(), `checkout "co_1" not found`) {
		t.Fatalf("restart with a deleted checkout = %v", err)
	}
}

func TestListIsNewestFirstScopedAndFiltered(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "out"}, nil
	}))
	a := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"a"}}).ID)
	b, _, err := f.svc.Create(context.Background(), Caller{Name: "project:atlas"}, api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(b.ID)
	stale, _, _ := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	f.svc.Store.Update(stale.ID, func(r *runstate.Record) error { r.Status = runstate.StatusUnfinished; return nil })

	all, err := f.svc.List(ListFilter{})
	if err != nil || len(all.Runs) != 3 || all.Runs[0].ID != stale.ID || all.Runs[1].ID != b.ID || all.Runs[2].ID != a.ID {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if all.Runs[2].Stdout != "" {
		t.Fatalf("list carries output: %+v", all.Runs[2])
	}
	if mine, _ := f.svc.List(ListFilter{Owner: "project:atlas"}); len(mine.Runs) != 1 || mine.Runs[0].ID != b.ID {
		t.Fatalf("owner filter = %+v", mine)
	}
	if lost, _ := f.svc.List(ListFilter{Status: api.RunUnfinished}); len(lost.Runs) != 1 || lost.Runs[0].ID != stale.ID {
		t.Fatalf("status filter = %+v", lost)
	}
	if one, _ := f.svc.List(ListFilter{Limit: 1}); len(one.Runs) != 1 || one.Runs[0].ID != stale.ID {
		t.Fatalf("limit = %+v", one)
	}
	var reqErr *RequestError
	if _, err := f.svc.List(ListFilter{Status: "done"}); !errors.As(err, &reqErr) {
		t.Fatalf("bad status = %v", err)
	}
	if none, err := New(f.svc.Store, nil, Config{}).List(ListFilter{Owner: "nobody"}); err != nil || len(none.Runs) != 0 || none.Runs == nil {
		t.Fatalf("empty list = %+v, %v", none, err)
	}
}

func TestReconcileSettlesRunningStepsToo(t *testing.T) {
	f := newFixture(t, blocking())
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Store.Update(rec.ID, func(r *runstate.Record) error { r.Status = runstate.StatusRunning; return nil })
	steps := agentstep.Store{RunDir: f.svc.Store.Path(rec.ID)}
	for _, st := range []string{api.StepSucceeded, api.StepRunning} {
		if _, err := steps.Create(api.Step{Run: rec.ID, Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := f.svc.Reconcile()
	if err != nil || len(changed) != 1 {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
	list, err := f.svc.Steps(rec.ID)
	if err != nil || len(list.Steps) != 2 || list.Steps[0].Status != api.StepSucceeded || list.Steps[1].Status != api.StepUnfinished || list.Steps[1].Reason != ReconcileReason {
		t.Fatalf("steps after reconcile = %+v, %v", list, err)
	}
}
