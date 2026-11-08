package serve

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

type runsLauncher func(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error)

func (l runsLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	return l(ctx, spec)
}

// newRunsServer wires a run service with a fake launcher onto a family
// server; the workspace root holds one directory, "proj".
func newRunsServer(t *testing.T, launcher runsLauncher) (*famServer, string) {
	t.Helper()
	f := newFamServer(t)
	store, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ws := filepath.Join(root, "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	backend := &runs.Backend{Name: "fake", Launcher: launcher, Capabilities: runs.Capabilities{NetworkNone: true}}
	f.srv.Runs = runs.New(store, backend, runs.Config{Image: "img", WorkspaceRoots: []string{root}})
	return f, ws
}

func TestRunsRoutesAnswerNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{Command: []string{"x"}}, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodGet, api.RunPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
	f.expectCode(http.MethodPost, api.RunCancelPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunsWithoutABackendStillServesRecords(t *testing.T) {
	f := newFamServer(t)
	store, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Runs = runs.New(store, nil, runs.Config{})
	rec, _, err := store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{Command: []string{"x"}}, http.StatusNotImplemented, api.CodeNotSupported)
	var run api.ContainedRun
	f.expect(http.MethodGet, api.RunPath(rec.ID), nil, http.StatusOK, &run)
	if run.ID != rec.ID || run.Status != api.RunAccepted {
		t.Fatalf("GET = %+v", run)
	}
}

func TestRunsCreateWaitsAndReturnsTheRecord(t *testing.T) {
	f, ws := newRunsServer(t, func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "ran " + spec.Command[0]}, nil
	})
	req := api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"make", "test"}, IdempotencyKey: "k"}
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, req, http.StatusOK, &run)
	if run.Status != api.RunCompleted || run.Stdout != "ran make" || run.ExitCode == nil || *run.ExitCode != 0 || run.Owner != "operator" {
		t.Fatalf("POST = %+v", run)
	}
	var again api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, req, http.StatusOK, &again)
	if again.ID != run.ID {
		t.Fatalf("idempotent replay returned %s, want %s", again.ID, run.ID)
	}
	var got api.ContainedRun
	f.expect(http.MethodGet, api.RunPath(run.ID), nil, http.StatusOK, &got)
	if got.ID != run.ID || got.Status != api.RunCompleted {
		t.Fatalf("GET = %+v", got)
	}
	var late api.ContainedRun
	f.expect(http.MethodPost, api.RunCancelPath(run.ID), nil, http.StatusOK, &late)
	if !late.AlreadyFinished || late.Status != api.RunCompleted {
		t.Fatalf("late cancel = %+v", late)
	}
}

func TestRunsDetachAndCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	f, ws := newRunsServer(t, func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return sandbox.Result{ExitCode: -1}, ctx.Err()
	})
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"sleep"}, Detach: true}, http.StatusAccepted, &run)
	if run.Status != api.RunAccepted || run.ID == "" {
		t.Fatalf("detached = %+v", run)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("launcher never started")
	}
	var cancelled api.ContainedRun
	f.expect(http.MethodPost, api.RunCancelPath(run.ID), nil, http.StatusOK, &cancelled)
	if cancelled.Status != api.RunCancelled || cancelled.AlreadyFinished || cancelled.ExitCode != nil {
		t.Fatalf("cancel = %+v", cancelled)
	}
}

func TestRunsErrorCodes(t *testing.T) {
	f, ws := newRunsServer(t, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	})
	f.expectCode(http.MethodPost, api.PathRuns, "{not json", http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Profile: api.RunProfileAgent}, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Profile: api.RunProfileAgent}, http.StatusNotImplemented, api.CodeUnsupported)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Network: api.RunNetworkRestricted}, http.StatusNotImplemented, api.CodeUnsupported)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{Checkout: "chk_1", Command: []string{"x"}}, http.StatusNotImplemented, api.CodeUnsupported)
	f.expectCode(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: "/elsewhere", Command: []string{"x"}}, http.StatusForbidden, api.CodeForbidden)
	f.expectCode(http.MethodGet, api.RunPath("run_missing"), nil, http.StatusNotFound, api.CodeNotFound)
	f.expectCode(http.MethodPost, api.RunCancelPath("run_missing"), nil, http.StatusNotFound, api.CodeNotFound)
}
