package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
	openshelltypes "github.com/ai-openshell/sdk-go/openshell/v1/types"
)

// fakeOpenShellLogs scripts the read side of the gateway: a sandbox list,
// one GetLogs answer and one Get answer per poll.
type fakeOpenShellLogs struct {
	mu        sync.Mutex
	list      []*openshell.Sandbox
	listErr   error
	selectors []string
	logs      [][]openshell.LogLine
	// logErrs[i] is returned by the i-th GetLogs call when set.
	logErrs []error
	// nilLogs makes every GetLogs call answer (nil, nil).
	nilLogs  bool
	since    []time.Time
	lines    []uint32
	gets     []*openshell.Sandbox
	getErr   error
	logCalls int
	getCalls int
	closed   int
}

func (f *fakeOpenShellLogs) Sandboxes() openshellSandboxes { return f }
func (f *fakeOpenShellLogs) Exec() openshellExec           { return nil }
func (f *fakeOpenShellLogs) Health() openshellHealth       { return nil }
func (f *fakeOpenShellLogs) Config() openshellConfig       { return nil }
func (f *fakeOpenShellLogs) Close() error                  { f.closed++; return nil }

func (f *fakeOpenShellLogs) Create(context.Context, string, string, *openshell.SandboxSpec, ...openshell.CreateOption) (*openshell.Sandbox, error) {
	return nil, nil
}

func (f *fakeOpenShellLogs) WaitReady(context.Context, string, string, ...openshell.WaitOptions) (*openshell.Sandbox, error) {
	return nil, nil
}

func (f *fakeOpenShellLogs) Delete(context.Context, string, string, ...openshell.DeleteOptions) (*openshell.DeletionResult, error) {
	return nil, nil
}

func (f *fakeOpenShellLogs) ListAll(_ context.Context, _ string, opts ...openshell.ListOptions) ([]*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range opts {
		f.selectors = append(f.selectors, o.LabelSelector)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeOpenShellLogs) Get(context.Context, string, string) (*openshell.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	i := f.getCalls
	f.getCalls++
	if i >= len(f.gets) {
		i = len(f.gets) - 1
	}
	return f.gets[i], nil
}

func (f *fakeOpenShellLogs) GetLogs(_ context.Context, _, _ string, opts ...openshell.LogOption) (*openshell.LogResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg := openshelltypes.ApplyLogOptions(opts)
	f.since = append(f.since, cfg.Since())
	f.lines = append(f.lines, cfg.Lines())
	i := f.logCalls
	f.logCalls++
	if i < len(f.logErrs) && f.logErrs[i] != nil {
		return nil, f.logErrs[i]
	}
	if f.nilLogs {
		return nil, nil
	}
	if i >= len(f.logs) {
		return &openshell.LogResult{}, nil
	}
	return &openshell.LogResult{Lines: f.logs[i]}, nil
}

func openshellLogLauncher(f *fakeOpenShellLogs) *OpenShellLauncher {
	return &OpenShellLauncher{
		Options:   OpenShellOptions{GatewayURL: "gw:8080"},
		newClient: func(OpenShellOptions) (openshellClient, error) { return f, nil },
	}
}

func osSandbox(name string, phase openshell.SandboxPhase) *openshell.Sandbox {
	return &openshell.Sandbox{Name: name, Status: openshell.SandboxStatus{Phase: phase}}
}

func TestOpenShellLatestSandboxPicksNewestForAgent(t *testing.T) {
	launch := LaunchName("deps")
	older := openshellSandboxName(launch, time.Unix(1700000000, 0))
	newer := openshellSandboxName(launch, time.Unix(1700000100, 0))
	deleting := osSandbox(openshellSandboxName(launch, time.Unix(1700000200, 0)), openshell.SandboxReady)
	now := time.Now()
	deleting.DeletionTimestamp = &now
	f := &fakeOpenShellLogs{list: []*openshell.Sandbox{
		osSandbox(older, openshell.SandboxReady),
		osSandbox(newer, openshell.SandboxProvisioning),
		deleting,
		osSandbox(openshellSandboxName(LaunchName("deps-bot"), time.Unix(1700000300, 0)), openshell.SandboxReady),
		osSandbox("unrelated", openshell.SandboxReady),
	}}
	info, found, err := openshellLogLauncher(f).LatestSandbox(context.Background(), launch)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if info.Name != newer || info.Phase != string(openshell.SandboxProvisioning) || info.Finished() {
		t.Fatalf("info = %+v, want the newest live sandbox %s", info, newer)
	}
	if len(f.selectors) != 1 || f.selectors[0] != openshellManagedLabel+"="+openshellManagedValue {
		t.Fatalf("list must be restricted to hive's sandboxes, got %v", f.selectors)
	}
	if f.closed != 1 {
		t.Fatalf("client closed %d times", f.closed)
	}

	_, found, err = openshellLogLauncher(&fakeOpenShellLogs{}).LatestSandbox(context.Background(), launch)
	if err != nil || found {
		t.Fatalf("no sandbox: found=%v err=%v", found, err)
	}
}

func TestOpenShellLatestSandboxNeedsGateway(t *testing.T) {
	l := &OpenShellLauncher{}
	if _, _, err := l.LatestSandbox(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "gateway_url") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenShellStreamLogsFollowsUntilExitWithoutDuplicates(t *testing.T) {
	t1 := time.Date(2026, 11, 11, 6, 0, 1, 0, time.UTC)
	t2 := t1.Add(time.Second)
	t3 := t2.Add(time.Second)
	code := int32(3)
	done := osSandbox("sb", openshell.SandboxCompleted)
	done.Status.ExitCode = &code
	f := &fakeOpenShellLogs{
		logs: [][]openshell.LogLine{
			{{Timestamp: t1, Source: "sandbox", Level: "INFO", Message: "step 1"}, {Timestamp: t2, Source: "sandbox", Message: "step 2\n"}},
			// "since" is inclusive: the gateway repeats the t2 line.
			{{Timestamp: t2, Source: "sandbox", Message: "step 2\n"}, {Timestamp: t3, Message: "step 3"}},
			{{Timestamp: t3, Message: "step 3"}},
		},
		gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady), done},
	}
	var out strings.Builder
	info, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &out)
	if err != nil {
		t.Fatal(err)
	}
	want := t1.Format(time.RFC3339) + " [sandbox] INFO step 1\n" +
		t2.Format(time.RFC3339) + " [sandbox] step 2\n" +
		t3.Format(time.RFC3339) + " step 3\n"
	if out.String() != want {
		t.Fatalf("log =\n%s\nwant\n%s", out.String(), want)
	}
	if !info.Exited || info.ExitCode != 3 || !info.Finished() {
		t.Fatalf("info = %+v", info)
	}
	if len(f.since) < 2 || !f.since[0].IsZero() || !f.since[1].Equal(t2) {
		t.Fatalf("follow-up reads must ask only for new lines, got %v", f.since)
	}
	for i, n := range f.lines {
		if n != openshellLogLinesPerRead {
			t.Fatalf("read %d asked for %d lines, want the gateway's full %d-line buffer so none are lost between polls", i, n, openshellLogLinesPerRead)
		}
	}
}

func TestOpenShellStreamLogsEndsWhenSandboxIsDeleted(t *testing.T) {
	f := &fakeOpenShellLogs{
		logs:   [][]openshell.LogLine{{{Message: "last words"}}},
		getErr: &openshell.StatusError{Code: openshell.ErrorNotFound, Message: "gone"},
	}
	var out strings.Builder
	info, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "last words\n" || !info.Finished() || info.Exited {
		t.Fatalf("out=%q info=%+v", out.String(), info)
	}
}

func TestOpenShellStreamLogsStopsWithContext(t *testing.T) {
	f := &fakeOpenShellLogs{gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := openshellLogLauncher(f).StreamLogs(ctx, "sb", time.Hour, &strings.Builder{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled follow must report the context error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StreamLogs kept polling after the context ended")
	}
}

func TestOpenShellLogClientErrors(t *testing.T) {
	boom := errors.New("dial failed")
	l := &OpenShellLauncher{
		Options:   OpenShellOptions{GatewayURL: "gw:8080"},
		newClient: func(OpenShellOptions) (openshellClient, error) { return nil, boom },
	}
	if _, _, err := l.LatestSandbox(context.Background(), "x"); !errors.Is(err, boom) {
		t.Fatalf("LatestSandbox err = %v, want %v", err, boom)
	}
	if _, err := l.StreamLogs(context.Background(), "sb", time.Millisecond, &strings.Builder{}); !errors.Is(err, boom) {
		t.Fatalf("StreamLogs err = %v, want %v", err, boom)
	}

	// A client whose Sandboxes() cannot read logs is closed and refused.
	narrow := &fakeOpenShell{}
	l.newClient = func(OpenShellOptions) (openshellClient, error) { return narrow, nil }
	_, _, err := l.LatestSandbox(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "cannot read sandbox logs") {
		t.Fatalf("err = %v", err)
	}
	if !narrow.closed {
		t.Fatal("a refused client must still be closed")
	}
}

func TestOpenShellLatestSandboxListError(t *testing.T) {
	boom := errors.New("gateway down")
	f := &fakeOpenShellLogs{listErr: boom}
	_, found, err := openshellLogLauncher(f).LatestSandbox(context.Background(), "deps")
	if found || !errors.Is(err, boom) || !strings.Contains(err.Error(), "listing sandboxes") {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if f.closed != 1 {
		t.Fatalf("client closed %d times", f.closed)
	}
}

func TestOpenShellSandboxNameUnix(t *testing.T) {
	launch := LaunchName("deps")
	good := openshellSandboxName(launch, time.Unix(1700000000, 0))
	cases := []struct {
		name string
		want bool
	}{
		{good, true},
		{"nodash", false},
		{"other-deps-1700000000", false},
		{openshellNamePrefix + "deps-not.a.number", false},
		{openshellSandboxName(LaunchName("deps-bot"), time.Unix(1700000000, 0)), false},
	}
	for _, c := range cases {
		unix, ok := openshellSandboxNameUnix(launch, c.name)
		if ok != c.want {
			t.Errorf("%q: ok=%v want %v", c.name, ok, c.want)
		}
		if ok && unix != 1700000000 {
			t.Errorf("%q: unix=%d", c.name, unix)
		}
	}
}

func TestOpenShellSandboxInfoDeletingWithoutPhase(t *testing.T) {
	now := time.Now()
	s := &openshell.Sandbox{Name: "sb", DeletionTimestamp: &now}
	info := openshellSandboxInfo(s)
	if info.Phase != string(openshell.SandboxDeleting) || !info.Finished() || info.Exited {
		t.Fatalf("info = %+v", info)
	}
	// A reported phase wins over the deletion marker.
	s.Status.Phase = openshell.SandboxStopped
	if got := openshellSandboxInfo(s); got.Phase != string(openshell.SandboxStopped) || !got.Finished() {
		t.Fatalf("info = %+v", got)
	}
	if !(OpenShellSandboxInfo{Phase: string(openshell.SandboxError)}).Finished() {
		t.Fatal("error phase must be finished")
	}
}

func TestOpenShellStreamLogsSkipsStaleLinesAndNilResults(t *testing.T) {
	t1 := time.Date(2026, 11, 11, 6, 0, 1, 0, time.UTC)
	t2 := t1.Add(time.Second)
	f := &fakeOpenShellLogs{
		logs: [][]openshell.LogLine{
			{{Timestamp: t2, Message: "now"}},
			// The gateway replays an older line and the t2 line again.
			{{Timestamp: t1, Message: "stale"}, {Timestamp: t2, Message: "now"}},
		},
		gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady), osSandbox("sb", openshell.SandboxCompleted)},
	}
	var out strings.Builder
	info, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &out)
	if err != nil {
		t.Fatal(err)
	}
	if want := t2.Format(time.RFC3339) + " now\n"; out.String() != want {
		t.Fatalf("log = %q, want %q", out.String(), want)
	}
	if !info.Finished() || info.Exited {
		t.Fatalf("info = %+v", info)
	}

	// A nil LogResult is an empty read, and poll <= 0 falls back to the default.
	f = &fakeOpenShellLogs{nilLogs: true, gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxCompleted)}}
	out.Reset()
	if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", 0, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || f.logCalls != 2 {
		t.Fatalf("out=%q logCalls=%d", out.String(), f.logCalls)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestOpenShellStreamLogsReportsErrors(t *testing.T) {
	boom := errors.New("boom")
	notFound := &openshell.StatusError{Code: openshell.ErrorNotFound, Message: "gone"}

	t.Run("get", func(t *testing.T) {
		f := &fakeOpenShellLogs{getErr: boom}
		if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &strings.Builder{}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("logs", func(t *testing.T) {
		f := &fakeOpenShellLogs{logErrs: []error{boom}, gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady)}}
		info, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &strings.Builder{})
		if !errors.Is(err, boom) || info.Phase != string(openshell.SandboxReady) {
			t.Fatalf("err=%v info=%+v", err, info)
		}
	})
	t.Run("logs not found is not fatal while the sandbox lives", func(t *testing.T) {
		f := &fakeOpenShellLogs{
			logErrs: []error{notFound, nil, nil},
			logs:    [][]openshell.LogLine{nil, {{Message: "late"}}},
			gets:    []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady), osSandbox("sb", openshell.SandboxCompleted)},
		}
		var out strings.Builder
		if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != "late\n" {
			t.Fatalf("out = %q", out.String())
		}
	})
	t.Run("final drain", func(t *testing.T) {
		f := &fakeOpenShellLogs{logErrs: []error{nil, boom}, gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxCompleted)}}
		if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &strings.Builder{}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		f = &fakeOpenShellLogs{logErrs: []error{nil, notFound}, gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxCompleted)}}
		if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, &strings.Builder{}); err != nil {
			t.Fatalf("a sandbox deleted during the final drain is not an error, got %v", err)
		}
	})
	t.Run("writer", func(t *testing.T) {
		f := &fakeOpenShellLogs{
			logs: [][]openshell.LogLine{{{Message: "x"}}},
			gets: []*openshell.Sandbox{osSandbox("sb", openshell.SandboxReady)},
		}
		if _, err := openshellLogLauncher(f).StreamLogs(context.Background(), "sb", time.Millisecond, failingWriter{boom}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}
