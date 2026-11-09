package runs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// withCmdLog turns the fixture's command log on with the file opener.
func withCmdLog(f *fixture) *fixture {
	f.svc.CmdLog = cmdlog.FileOpener(cmdlog.Policy{})
	return f
}

func TestRunnerAppendsTheDispatchedCommandToTheLog(t *testing.T) {
	f := withCmdLog(newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "hello\n", Stderr: "token=ghp_0123456789abcdefghijklmnopqrstuvwxyz"}, nil
	})))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"echo", "hello"}}).ID)
	page, err := f.svc.Commands(run.ID, 0, 0)
	if err != nil || page.Run != run.ID || len(page.Commands) != 1 || page.More {
		t.Fatalf("Commands = %+v, %v", page, err)
	}
	rec := page.Commands[0]
	if rec.Seq != 1 || rec.Capture != api.CommandCaptureRunner || rec.Cwd != sandbox.DefaultWorkspaceMount || len(rec.Command) != 2 || rec.Command[1] != "hello" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Status != api.CommandCompleted || rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Stdout.Head != "hello\n" || rec.Stderr.Bytes == 0 {
		t.Fatalf("outcome = %+v", rec)
	}
	// The runtime scrubbed the token before it reached the file.
	raw, err := os.ReadFile(filepath.Join(f.svc.Store.Path(run.ID), cmdlog.FileName))
	if err != nil || len(raw) == 0 || bytes.Contains(raw, []byte("ghp_")) {
		t.Fatalf("log file = %q, %v", raw, err)
	}
}

func TestLogStatusFollowsTheOutcome(t *testing.T) {
	cases := []struct {
		name     string
		launcher *fakeLauncher
		timeout  int64
		cancel   bool
		want     string
		exit     *int
	}{
		{name: "non-zero exit is failed", want: api.CommandFailed, exit: ptr(3), launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 3}, errors.New("exit status 3")
		})},
		{name: "unknown exit is unfinished", want: api.CommandUnfinished, launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: -1}, errors.New("pod vanished")
		})},
		{name: "refusal is failed", want: api.CommandFailed, launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("no image")
		})},
		{name: "timeout is failed", want: api.CommandFailed, timeout: 20, launcher: blocking()},
		{name: "cancel is cancelled", want: api.CommandCancelled, cancel: true, launcher: blocking()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := withCmdLog(newFixture(t, tc.launcher))
			run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}, TimeoutMs: tc.timeout})
			if tc.cancel {
				f.spec()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if _, err := f.svc.Cancel(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.wait(run.ID)
			}
			page, err := f.svc.Commands(run.ID, 0, 0)
			if err != nil || len(page.Commands) != 1 {
				t.Fatalf("Commands = %+v, %v", page, err)
			}
			rec := page.Commands[0]
			if rec.Status != tc.want || (tc.exit == nil) != (rec.ExitCode == nil) || (tc.exit != nil && *tc.exit != *rec.ExitCode) || rec.EndedAt == nil {
				t.Fatalf("record = %+v want %s exit=%v", rec, tc.want, tc.exit)
			}
			// Every outcome is terminal, unfinished included: a follower moves on.
			if page.NextSeq != rec.Seq || page.More {
				t.Fatalf("page = %+v", page)
			}
		})
	}
}

func TestCommandsWithoutALogIsAnEmptyPage(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	// CmdLog unset: the run executes, nothing is logged, the page is empty.
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	page, err := f.svc.Commands(run.ID, 3, 0)
	if err != nil || page.Run != run.ID || len(page.Commands) != 0 || page.NextSeq != 3 || page.More {
		t.Fatalf("empty = %+v, %v", page, err)
	}
	if _, err := f.svc.Commands("run_missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
}

// A command the previous process dispatched and never ended is ended
// unfinished at restart, so a follower's next_seq is not pinned on it
// forever (#17410).
func TestReconcileEndsTheCommandARunLeftInFlight(t *testing.T) {
	f := withCmdLog(newFixture(t, blocking()))
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	log, err := f.svc.CmdLog(f.svc.Store.Path(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Started(cmdlog.Start{Command: []string{"make"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	page, err := f.svc.Commands(rec.ID, 0, 0)
	if err != nil || len(page.Commands) != 1 || page.NextSeq != 1 || page.More {
		t.Fatalf("Commands = %+v, %v", page, err)
	}
	if got := page.Commands[0]; got.Status != api.CommandUnfinished || got.EndedAt == nil || got.ExitCode != nil || got.Reason != cmdlog.OrphanReason {
		t.Fatalf("record = %+v", got)
	}
}

func TestAnOpenerFailureDoesNotStopTheRun(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "ran"}, nil
	}))
	f.svc.CmdLog = func(string) (cmdlog.Recorder, error) { return nil, errors.New("disk full") }
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunCompleted || run.Stdout != "ran" {
		t.Fatalf("run = %+v", run)
	}
	page, err := f.svc.Commands(run.ID, 0, 0)
	if err != nil || len(page.Commands) != 0 {
		t.Fatalf("Commands = %+v, %v", page, err)
	}
}

func TestCommandStatusTable(t *testing.T) {
	for _, tc := range []struct {
		status string
		exit   *int
		want   string
	}{
		{runstate.StatusCompleted, ptr(0), api.CommandCompleted},
		{runstate.StatusCompleted, ptr(1), api.CommandFailed},
		{runstate.StatusCompleted, nil, api.CommandFailed},
		{runstate.StatusCancelled, nil, api.CommandCancelled},
		{runstate.StatusUnfinished, nil, api.CommandUnfinished},
		{runstate.StatusFailed, nil, api.CommandFailed},
		{runstate.StatusFailed, ptr(137), api.CommandFailed},
	} {
		if got := commandStatus(tc.status, tc.exit); got != tc.want {
			t.Errorf("commandStatus(%s, %v) = %s want %s", tc.status, tc.exit, got, tc.want)
		}
	}
}
