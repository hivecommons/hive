package runs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/signals"
)

// withSignals turns the fixture's turn signals on.
func withSignals(f *fixture) *fixture {
	f.svc.Signals = signals.NewBook(signals.Policy{})
	return f
}

// kinds lists the run's signals as kind/source, and decodes the last one's
// data when it is done.
func (f *fixture) kinds(id string) ([]string, api.SignalDoneData, string) {
	f.t.Helper()
	page, err := f.svc.ReadSignals(id, 0, 0)
	if err != nil || page.Run != id {
		f.t.Fatalf("ReadSignals = %+v, %v", page, err)
	}
	var out []string
	var done api.SignalDoneData
	var msg string
	for _, sig := range page.Signals {
		out = append(out, sig.Kind+"/"+sig.Source)
		if sig.Kind == api.SignalDone {
			if err := json.Unmarshal(sig.Data, &done); err != nil {
				f.t.Fatalf("done data %s: %v", sig.Data, err)
			}
			msg = sig.Message
		}
	}
	return out, done, msg
}

func TestRunnerSignalsStartedAndDoneOnEveryTerminalPath(t *testing.T) {
	launched := []string{"started/runner", "done/runner"}
	cases := []struct {
		name     string
		launcher *fakeLauncher
		timeout  int64
		cancel   bool
		status   string
		exit     *int
		timedOut bool
		reason   string
	}{
		{name: "completed", status: api.RunCompleted, exit: ptr(0), launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{}, nil
		})},
		{name: "non-zero exit", status: api.RunCompleted, exit: ptr(3), reason: "exit status 3", launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 3}, errors.New("exit status 3")
		})},
		{name: "refused", status: api.RunFailed, reason: "no image", launcher: newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("no image")
		})},
		{name: "timed out", status: api.RunFailed, timeout: 20, timedOut: true, reason: "timed out", launcher: blocking()},
		{name: "cancelled", status: api.RunCancelled, cancel: true, reason: "cancelled by the caller", launcher: blocking()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := withSignals(newFixture(t, tc.launcher))
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
			got, done, msg := f.kinds(run.ID)
			if !slices.Equal(got, launched) {
				t.Fatalf("signals = %v", got)
			}
			if done.Status != tc.status || done.TimedOut != tc.timedOut || (tc.exit == nil) != (done.ExitCode == nil) || (tc.exit != nil && *tc.exit != *done.ExitCode) || msg != tc.reason {
				t.Fatalf("done = %+v %q, want %s exit=%v timed_out=%v %q", done, msg, tc.status, tc.exit, tc.timedOut, tc.reason)
			}
		})
	}
}

func TestProvisioningFailureIsDoneWithoutStarted(t *testing.T) {
	f := withSignals(newFixture(t, blocking()))
	wt := &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}

	f.svc.Provisioner = provisionFunc(func(context.Context, string, api.ContainedRunWorktree) (string, error) {
		return "", errors.New("clone refused")
	})
	run := f.wait(f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}}).ID)
	got, done, msg := f.kinds(run.ID)
	if !slices.Equal(got, []string{"done/runner"}) || done.Status != api.RunFailed || done.ExitCode != nil || msg != "provisioning the worktree: clone refused" {
		t.Fatalf("failed provision: %v %+v %q", got, done, msg)
	}

	f.svc.Provisioner = provisionFunc(func(ctx context.Context, _ string, _ api.ContainedRunWorktree) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	run = f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}})
	if _, err := f.svc.Cancel(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	got, done, _ = f.kinds(run.ID)
	if !slices.Equal(got, []string{"done/runner"}) || done.Status != api.RunCancelled {
		t.Fatalf("cancelled provision: %v %+v", got, done)
	}
}

func TestReconcileClosesTheSignalsOfARunLeftInFlight(t *testing.T) {
	f := withSignals(newFixture(t, blocking()))
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Signals.Append(rec.ID, f.svc.Store.Path(rec.ID), signals.Signal{Kind: api.SignalStarted, Source: api.SignalSourceRunner}); err != nil {
		t.Fatal(err)
	}
	// A restarted server: a new book over the same store.
	f.svc.Signals = signals.NewBook(signals.Policy{})
	if _, err := f.svc.Reconcile(); err != nil {
		t.Fatal(err)
	}
	got, done, msg := f.kinds(rec.ID)
	if !slices.Equal(got, []string{"started/runner", "done/runner"}) || done.Status != api.RunUnfinished || msg != ReconcileReason {
		t.Fatalf("reconciled: %v %+v %q", got, done, msg)
	}
}

func TestAppendSignalChecksTheServiceAndTheRun(t *testing.T) {
	f := newFixture(t, blocking())
	rec, _, err := f.svc.Store.Create(runstate.Record{Owner: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	thinking := signals.Signal{Kind: api.SignalThinking, Source: api.SignalSourceCaller}
	if _, err := f.svc.AppendSignal(rec.ID, thinking); !errors.Is(err, ErrNoSignals) {
		t.Fatalf("no book: %v", err)
	}
	// Without a book the runner keeps no signals and the page is empty.
	page, err := f.svc.ReadSignals(rec.ID, 7, 0)
	if err != nil || page.Run != rec.ID || len(page.Signals) != 0 || page.NextSeq != 7 {
		t.Fatalf("empty page = %+v, %v", page, err)
	}
	withSignals(f)
	if _, err := f.svc.AppendSignal("run_missing", thinking); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := f.svc.ReadSignals("run_missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read unknown run: %v", err)
	}
	sig, err := f.svc.AppendSignal(rec.ID, thinking)
	if err != nil || sig.Seq != 1 || sig.Source != api.SignalSourceCaller {
		t.Fatalf("append = %+v, %v", sig, err)
	}
	// A run that ended before its done line (or on a server without
	// signals) is closed to callers and gates all the same.
	if _, err := f.svc.Store.Update(rec.ID, func(r *runstate.Record) error { r.Status = runstate.StatusCompleted; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AppendSignal(rec.ID, thinking); !errors.Is(err, signals.ErrClosed) {
		t.Fatalf("terminal run: %v", err)
	}
}
