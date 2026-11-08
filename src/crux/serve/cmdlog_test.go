package serve

import (
	"context"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestRunsCmdlogRouteAnswersNotSupportedWithoutAService(t *testing.T) {
	f := newFamServer(t)
	f.expectCode(http.MethodGet, api.RunCmdlogPath("run_x"), nil, http.StatusNotImplemented, api.CodeNotSupported)
}

func TestRunsCmdlogListsWhatTheRunnerDispatched(t *testing.T) {
	f, ws := newRunsServer(t, func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "ran " + spec.Command[0], ExitCode: 2}, nil
	})
	f.srv.Runs.CmdLog = cmdlog.FileOpener(cmdlog.Policy{})
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"make", "test"}}, http.StatusOK, &run)

	var page api.CommandPage
	f.expect(http.MethodGet, api.RunCmdlogPath(run.ID), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Commands) != 1 || page.NextSeq != 1 || page.More {
		t.Fatalf("page = %+v", page)
	}
	rec := page.Commands[0]
	if rec.Seq != 1 || rec.Status != api.CommandFailed || rec.ExitCode == nil || *rec.ExitCode != 2 || rec.Capture != api.CommandCaptureRunner || rec.Stdout.Head != "ran make" {
		t.Fatalf("record = %+v", rec)
	}
	// A follower resumes after the last seq it saw and gets nothing new.
	var next api.CommandPage
	f.expect(http.MethodGet, api.RunCmdlogQuery(run.ID, page.NextSeq, 10), nil, http.StatusOK, &next)
	if len(next.Commands) != 0 || next.NextSeq != 1 {
		t.Fatalf("resumed page = %+v", next)
	}
	// Paging parameters are validated.
	f.expectCode(http.MethodGet, api.RunCmdlogPath(run.ID)+"?since_seq=-1", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunCmdlogPath(run.ID)+"?since_seq=abc", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunCmdlogPath(run.ID)+"?limit=0", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunCmdlogPath(run.ID)+"?limit=x", nil, http.StatusBadRequest, api.CodeBadRequest)
	f.expectCode(http.MethodGet, api.RunCmdlogPath("run_000000000000"), nil, http.StatusNotFound, api.CodeNotFound)
}

func TestRunsCmdlogIsEmptyWhenNoLogIsKept(t *testing.T) {
	f, ws := newRunsServer(t, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	})
	var run api.ContainedRun
	f.expect(http.MethodPost, api.PathRuns, api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}}, http.StatusOK, &run)
	var page api.CommandPage
	f.expect(http.MethodGet, api.RunCmdlogQuery(run.ID, 4, 0), nil, http.StatusOK, &page)
	if page.Run != run.ID || len(page.Commands) != 0 || page.NextSeq != 4 {
		t.Fatalf("page = %+v", page)
	}
}
