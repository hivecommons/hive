package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// withHeldRuns is withRuns with the launched channel exposed: each launch
// reports on it and then holds until cleanup or its context ends.
func (f *gateFixture) withHeldRuns() (string, <-chan struct{}) {
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
	f.s.Runs = runs.New(store, backend, runs.Config{Image: "img", WorkspaceRoots: []string{root}})
	f.h = f.s.Handler()
	return ws, launched
}

const stepSchema = `{"type":"object","required":["verdict"]}`

func stepReq(prompt string) api.StepRequest {
	return api.StepRequest{Backend: "process", Prompt: prompt, OutputSchema: json.RawMessage(stepSchema), Command: []string{"./ask"}}
}

func TestStepsRoutesAnswerNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodPost, api.RunStepsPath("run_x"), stepReq("p"), http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodGet, api.RunStepsPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodGet, api.RunStepPath("run_x", 1), nil, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestSessionStepsOverHTTP(t *testing.T) {
	f, ws := newRunsServer(t, func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: `{"verdict":"pass"}`}, nil
	})
	// A session is answered as accepted whether or not detach was asked.
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Profile: api.RunProfileAgent, Network: api.RunNetworkNone}, http.StatusAccepted, &run)
	if run.Status != api.RunAccepted || run.Profile != api.RunProfileAgent {
		t.Fatalf("session = %+v", run)
	}
	var step api.Step
	f.expect(http.MethodPost, api.RunStepsPath(run.ID), stepReq("first"), http.StatusCreated, &step)
	if step.N != 1 || step.Status != api.StepSucceeded || string(step.Answer) != `{"verdict":"pass"}` {
		t.Fatalf("step = %+v", step)
	}
	f.expectCode(http.MethodPost, api.RunStepsPath(run.ID), api.StepRequest{Backend: "process"}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.RunStepsPath(run.ID), "{", http.StatusBadRequest, api.CodeBadRequest)
	unsupported := stepReq("p")
	unsupported.Tools = []string{"Bash"}
	f.expectCode(http.MethodPost, api.RunStepsPath(run.ID), unsupported, http.StatusNotImplemented, api.CodeUnsupported)

	var list api.StepList
	f.expect(http.MethodGet, api.RunStepsPath(run.ID), nil, http.StatusOK, &list)
	if list.Run != run.ID || len(list.Steps) != 2 || list.Steps[1].Status != api.StepUnsupported {
		t.Fatalf("list = %+v", list)
	}
	var got api.Step
	f.expect(http.MethodGet, api.RunStepPath(run.ID, 1), nil, http.StatusOK, &got)
	if got.N != 1 || got.Status != api.StepSucceeded {
		t.Fatalf("get = %+v", got)
	}
	f.expectCode(http.MethodGet, api.RunStepPath(run.ID, 9), nil, http.StatusNotFound, api.CodeNotFound)
	f.expectCode(http.MethodGet, api.PathRuns+"/"+run.ID+"/steps/zero", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunStepsPath("run_000000000000"), nil, http.StatusNotFound, api.CodeNotFound)

	var closed api.ContainedRun
	f.expect(http.MethodPost, api.RunCancelPath(run.ID), nil, http.StatusOK, &closed)
	if closed.Status != api.RunCancelled {
		t.Fatalf("closed = %+v", closed)
	}
	f.expectCode(http.MethodPost, api.RunStepsPath(run.ID), stepReq("late"), http.StatusConflict, api.CodeNotSession)

	// A finished experiment is not a session either.
	var exp api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &exp)
	f.expectCode(http.MethodPost, api.RunStepsPath(exp.ID), stepReq("p"), http.StatusConflict, api.CodeNotSession)
}

func TestConcurrentStepIsBusyAndStepsAreOwned(t *testing.T) {
	f := newGateFixture(t)
	ws, launched := f.withHeldRuns()
	var run api.ContainedRun
	if code, e := f.call("atlas", http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Profile: api.RunProfileAgent, Network: api.RunNetworkNone}, &run); code != http.StatusAccepted {
		t.Fatalf("session: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodGet, api.RunStepsPath(run.ID), nil, nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon on atlas's session: %d %+v", code, e)
	}
	if code, e := f.call("beacon", http.MethodPost, api.RunStepsPath(run.ID), stepReq("p"), nil); code != http.StatusForbidden || e.Code != api.CodeForbidden {
		t.Fatalf("beacon posting to atlas's session: %d %+v", code, e)
	}
	// The holding launcher keeps the first step in flight until cleanup.
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.call("atlas", http.MethodPost, api.RunStepsPath(run.ID), stepReq("slow"), nil)
	}()
	<-launched
	if code, e := f.call("atlas", http.MethodPost, api.RunStepsPath(run.ID), stepReq("second"), nil); code != http.StatusConflict || e.Code != api.CodeStepBusy {
		t.Fatalf("concurrent step: %d %+v", code, e)
	}
	var list api.StepList
	if code, _ := f.call("operator", http.MethodGet, api.RunStepsPath(run.ID), nil, &list); code != http.StatusOK || len(list.Steps) != 1 || list.Steps[0].Status != api.StepRunning {
		t.Fatalf("list during a step: %d %+v", code, list)
	}
	if code, _ := f.call("atlas", http.MethodPost, api.RunCancelPath(run.ID), nil, nil); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	<-done
	var step api.Step
	if code, _ := f.call("atlas", http.MethodGet, api.RunStepPath(run.ID, 1), nil, &step); code != http.StatusOK || step.Status != api.StepCancelled {
		t.Fatalf("cancelled step: %d %+v", code, step)
	}
}
