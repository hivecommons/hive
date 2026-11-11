package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
)

// fakeOpenShellLogs scripts the read side of the gateway: a sandbox list,
// one GetLogs answer and one Get answer per poll.
type fakeOpenShellLogs struct {
	mu        sync.Mutex
	list      []*openshell.Sandbox
	selectors []string
	logs      [][]openshell.LogLine
	since     []time.Time
	gets      []*openshell.Sandbox
	getErr    error
	logCalls  int
	getCalls  int
	closed    int
}

func (f *fakeOpenShellLogs) Sandboxes() openshellSandboxes { return f }
func (f *fakeOpenShellLogs) Exec() openshellExec           { return nil }
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
	f.since = append(f.since, openshellLogSince(opts))
	i := f.logCalls
	f.logCalls++
	if i >= len(f.logs) {
		return &openshell.LogResult{}, nil
	}
	return &openshell.LogResult{Lines: f.logs[i]}, nil
}

func openshellLogSince(opts []openshell.LogOption) time.Time {
	// The SDK keeps the applied options private; WithLogSince is the only
	// option the reader passes, so its presence is what the fake records.
	if len(opts) == 0 {
		return time.Time{}
	}
	return time.Unix(1, 0)
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
	if len(f.since) < 2 || !f.since[0].IsZero() || f.since[1].IsZero() {
		t.Fatalf("follow-up reads must ask only for new lines, got %v", f.since)
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
