package runs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/signals"
)

// withSignals turns the fixture's signal log on and collects what the
// observer is told.
func withSignals(f *fixture) (*fixture, func() []api.Signal) {
	f.svc.SignalLog = signals.FileOpener(signals.Policy{})
	var mu sync.Mutex
	var seen []api.Signal
	f.svc.SignalObserver = func(_ string, sig api.Signal) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, sig)
	}
	return f, func() []api.Signal {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.Signal(nil), seen...)
	}
}

func kinds(sigs []api.Signal) []string {
	out := make([]string, len(sigs))
	for i, s := range sigs {
		out[i] = s.Kind
	}
	return out
}

func TestRunnerEmitsStartedAndDone(t *testing.T) {
	f, observed := withSignals(newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 3}, errors.New("exit status 3")
	})))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"make", "test"}}).ID)
	page, err := f.svc.Signals(run.ID, 0, 0)
	if err != nil || page.Run != run.ID || len(page.Signals) != 2 || page.More || page.NextSeq != 2 {
		t.Fatalf("Signals = %+v, %v", page, err)
	}
	started, done := page.Signals[0], page.Signals[1]
	if started.Kind != api.SignalStarted || started.Source != api.SignalSourceRunner || started.By != "" {
		t.Fatalf("started = %+v", started)
	}
	var sd startedData
	if json.Unmarshal(started.Data, &sd) != nil || len(sd.Command) != 2 || sd.Backend != "fake" || sd.Network != api.RunNetworkNone {
		t.Fatalf("started data = %s", started.Data)
	}
	var dd doneData
	if done.Kind != api.SignalDone || json.Unmarshal(done.Data, &dd) != nil || dd.Status != api.RunCompleted || dd.ExitCode == nil || *dd.ExitCode != 3 || dd.TimedOut {
		t.Fatalf("done = %+v data=%s", done, done.Data)
	}
	if got := observed(); len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("observer saw %+v", got)
	}
}

func TestEveryTerminalPathEndsWithDone(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f, _ := withSignals(newFixture(t, blocking()))
		run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}, TimeoutMs: 20}).ID)
		page, _ := f.svc.Signals(run.ID, 0, 0)
		var dd doneData
		if k := kinds(page.Signals); len(k) != 2 || k[1] != api.SignalDone || json.Unmarshal(page.Signals[1].Data, &dd) != nil || !dd.TimedOut || dd.Status != api.RunFailed {
			t.Fatalf("timeout signals = %+v", page.Signals)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f, _ := withSignals(newFixture(t, blocking()))
		run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}})
		f.spec()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := f.svc.Cancel(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		page, _ := f.svc.Signals(run.ID, 0, 0)
		var dd doneData
		if k := kinds(page.Signals); len(k) != 2 || k[1] != api.SignalDone || json.Unmarshal(page.Signals[1].Data, &dd) != nil || dd.Status != api.RunCancelled {
			t.Fatalf("cancel signals = %+v", page.Signals)
		}
	})
	t.Run("provision failure has no started", func(t *testing.T) {
		f, _ := withSignals(newFixture(t, blocking()))
		f.svc.Provisioner = provisionFunc(func(context.Context, string, api.ContainedRunWorktree) (string, error) {
			return "", errors.New("clone refused")
		})
		wt := &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}
		run := f.wait(f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}}).ID)
		page, _ := f.svc.Signals(run.ID, 0, 0)
		if k := kinds(page.Signals); len(k) != 1 || k[0] != api.SignalDone || page.Signals[0].Message != "provisioning the worktree: clone refused" {
			t.Fatalf("provision signals = %+v", page.Signals)
		}
	})
}

func TestEmitByACallerAndItsLimits(t *testing.T) {
	f, observed := withSignals(newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	})))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	rec, err := f.svc.Emit(run.ID, signals.Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller, By: "project:atlas", Message: "go test ./..."})
	if err != nil || rec.Seq != 3 || rec.By != "project:atlas" || rec.Source != api.SignalSourceCaller {
		t.Fatalf("emit = %+v, %v", rec, err)
	}
	if _, err := f.svc.Emit(run.ID, signals.Signal{Kind: api.SignalDone, Source: api.SignalSourceCaller}); !errors.Is(err, signals.ErrKindNotAllowed) {
		t.Fatalf("caller done = %v", err)
	}
	if _, err := f.svc.Emit("run_missing", signals.Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
	if got := observed(); len(got) != 3 || got[2].Kind != api.SignalTool {
		t.Fatalf("observer saw %+v", got)
	}
	// A page resumed after the runner's two signals holds only the caller's.
	page, err := f.svc.Signals(run.ID, 2, 0)
	if err != nil || len(page.Signals) != 1 || page.Signals[0].Seq != 3 || page.NextSeq != 3 {
		t.Fatalf("resumed = %+v, %v", page, err)
	}
}

func TestSignalsWithoutALogIsAnEmptyPageAndEmitIsNotConfigured(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunCompleted {
		t.Fatalf("run = %+v", run)
	}
	page, err := f.svc.Signals(run.ID, 4, 0)
	if err != nil || page.Run != run.ID || len(page.Signals) != 0 || page.NextSeq != 4 {
		t.Fatalf("empty = %+v, %v", page, err)
	}
	if _, err := f.svc.Signals("run_missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
	if _, err := f.svc.Emit(run.ID, signals.Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("emit without log = %v", err)
	}
	// An opener that fails never fails the run.
	f.svc.SignalLog = func(string) (signals.Recorder, error) { return nil, errors.New("disk full") }
	run = f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"y"}}).ID)
	if run.Status != api.RunCompleted {
		t.Fatalf("run with failing opener = %+v", run)
	}
	if _, err := f.svc.Emit(run.ID, signals.Signal{Kind: api.SignalTool, Source: api.SignalSourceCaller}); err == nil || errors.Is(err, ErrNotConfigured) {
		t.Fatalf("emit with failing opener = %v", err)
	}
}
