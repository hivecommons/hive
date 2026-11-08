package runs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agentstep"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

const stepSchema = `{"type":"object","required":["verdict"],"properties":{"verdict":{"enum":["pass","fail"]}}}`

// session opens an agent-profile run (network none, which the fake
// backend supports) and waits for it to be ready.
func (f *fixture) session() api.ContainedRun {
	f.t.Helper()
	run, created, err := f.svc.Create(context.Background(), operator, api.ContainedRunRequest{
		WorkspacePath: f.workspace(), Profile: api.RunProfileAgent, Network: api.RunNetworkNone, EnvAllowlist: []string{"KEEP"},
	})
	if err != nil || !created || run.Status != api.RunAccepted || run.Profile != api.RunProfileAgent {
		f.t.Fatalf("Create session = %+v %v %v", run, created, err)
	}
	return run
}

func stepReq(prompt string) api.StepRequest {
	return api.StepRequest{Backend: "process", Prompt: prompt, OutputSchema: json.RawMessage(stepSchema), Command: []string{"./ask"}}
}

func (f *fixture) cancel(id string) api.ContainedRun {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run, err := f.svc.Cancel(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

func TestSessionRunsStepsUntilCancelled(t *testing.T) {
	f := withCmdLog(withSignals(newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: `{"verdict":"pass"}`}, nil
	}))))
	run := f.session()
	// RunStep waits for the session to be ready, so no status polling.
	step, err := f.svc.RunStep(context.Background(), run.ID, stepReq("first"))
	if err != nil || step.N != 1 || step.Status != api.StepSucceeded || step.Run != run.ID {
		t.Fatalf("RunStep = %+v, %v", step, err)
	}
	spec := f.spec()
	if spec.Name != "crux-step-"+run.ID+"-1-1" || spec.Workspace != f.workspace() || spec.NetworkMode != api.RunNetworkNone || spec.Env["KEEP"] != "yes" || spec.Env["GITHUB_TOKEN"] != "" || spec.Image != "img:1" {
		t.Fatalf("spec = %+v", spec)
	}
	if got, err := f.svc.Get(run.ID); err != nil || got.Status != api.RunWaiting || got.Workdir != f.workspace() {
		t.Fatalf("between steps: %+v, %v", got, err)
	}
	if step, err := f.svc.RunStep(context.Background(), run.ID, stepReq("second")); err != nil || step.N != 2 {
		t.Fatalf("second RunStep = %+v, %v", step, err)
	}
	list, err := f.svc.Steps(run.ID)
	if err != nil || list.Run != run.ID || len(list.Steps) != 2 || list.Steps[1].N != 2 {
		t.Fatalf("Steps = %+v, %v", list, err)
	}
	if got, err := f.svc.Step(run.ID, 2); err != nil || got.Status != api.StepSucceeded {
		t.Fatalf("Step = %+v, %v", got, err)
	}
	if _, err := f.svc.Step(run.ID, 3); !errors.Is(err, agentstep.ErrNotFound) {
		t.Fatalf("Step(3) = %v", err)
	}
	if _, err := f.svc.Steps("run_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Steps(missing) = %v", err)
	}
	page, err := f.svc.Commands(run.ID, 0, 0)
	if err != nil || len(page.Commands) != 2 || page.Commands[0].Status != api.CommandCompleted {
		t.Fatalf("Commands = %+v, %v", page, err)
	}

	closed := f.cancel(run.ID)
	if closed.Status != api.RunCancelled || !strings.Contains(closed.Reason, "2 step(s)") {
		t.Fatalf("closed = %+v", closed)
	}
	kinds, done, _ := f.kinds(run.ID)
	if !slices.Equal(kinds, []string{"started/runner", "step/runner", "step/runner", "done/runner"}) || done.Status != api.RunCancelled {
		t.Fatalf("signals = %v %+v", kinds, done)
	}
	if _, err := f.svc.RunStep(context.Background(), run.ID, stepReq("late")); !errors.Is(err, ErrNotSession) {
		t.Fatalf("step after close = %v", err)
	}
	if again := f.cancel(run.ID); !again.AlreadyFinished {
		t.Fatalf("second cancel = %+v", again)
	}
}

func TestCancelEndsTheStepInFlightAndASecondStepIsBusy(t *testing.T) {
	f := withSignals(newFixture(t, blocking()))
	run := f.session()
	type result struct {
		step api.Step
		err  error
	}
	results := make(chan result, 1)
	go func() {
		step, err := f.svc.RunStep(context.Background(), run.ID, stepReq("slow"))
		results <- result{step, err}
	}()
	f.spec()
	if got, err := f.svc.Get(run.ID); err != nil || got.Status != api.RunRunning {
		t.Fatalf("during a step: %+v, %v", got, err)
	}
	if _, err := f.svc.RunStep(context.Background(), run.ID, stepReq("second")); !errors.Is(err, ErrStepBusy) {
		t.Fatalf("concurrent step = %v", err)
	}
	closed := f.cancel(run.ID)
	res := <-results
	if res.err != nil || res.step.Status != api.StepCancelled {
		t.Fatalf("step in flight = %+v, %v", res.step, res.err)
	}
	if closed.Status != api.RunCancelled || !strings.Contains(closed.Reason, "1 step(s)") {
		t.Fatalf("closed = %+v", closed)
	}
	select {
	case name := <-f.torn:
		if name != "crux-step-"+run.ID+"-1-1" {
			t.Fatalf("torn %q", name)
		}
	default:
		t.Fatal("the step's launch was not torn down")
	}
}

func TestStepsNeedAnOpenSession(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	exp := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if _, err := f.svc.RunStep(context.Background(), exp.ID, stepReq("p")); !errors.Is(err, ErrNotSession) {
		t.Fatalf("step on an experiment = %v", err)
	}
	if _, err := f.svc.RunStep(context.Background(), "run_missing", stepReq("p")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("step on a missing run = %v", err)
	}
	if list, err := f.svc.Steps(exp.ID); err != nil || len(list.Steps) != 0 {
		t.Fatalf("Steps of an experiment = %+v, %v", list, err)
	}
	// A malformed step is refused without a record; an unsupported one is
	// recorded and refused.
	run := f.session()
	defer f.cancel(run.ID)
	var reqErr *agentstep.RequestError
	if _, err := f.svc.RunStep(context.Background(), run.ID, api.StepRequest{Backend: "process"}); !errors.As(err, &reqErr) {
		t.Fatalf("malformed step = %v", err)
	}
	req := stepReq("p")
	req.Tools = []string{"Bash"}
	var unsupported *agentstep.UnsupportedError
	step, err := f.svc.RunStep(context.Background(), run.ID, req)
	if !errors.As(err, &unsupported) || step.N != 1 || step.Status != api.StepUnsupported {
		t.Fatalf("unsupported step = %+v, %v", step, err)
	}
	if got, err := f.svc.Get(run.ID); err != nil || got.Status != api.RunWaiting {
		t.Fatalf("after a refused step: %+v, %v", got, err)
	}
}

func TestReconcileClosesAWaitingSession(t *testing.T) {
	f := newFixture(t, blocking())
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Store.Update(rec.ID, func(r *runstate.Record) error { r.Status = runstate.StatusWaiting; return nil }); err != nil {
		t.Fatal(err)
	}
	changed, err := f.svc.Reconcile()
	if err != nil || len(changed) != 1 || changed[0].Status != runstate.StatusUnfinished {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
	if _, err := f.svc.RunStep(context.Background(), rec.ID, stepReq("p")); !errors.Is(err, ErrNotSession) {
		t.Fatalf("step on a reconciled run = %v", err)
	}
}
