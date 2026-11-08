package runs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
)

// fakeLauncher drives every classify path without a container runtime.
type fakeLauncher struct {
	run   func(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error)
	specs chan sandbox.LaunchSpec
}

func newFakeLauncher(run func(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error)) *fakeLauncher {
	return &fakeLauncher{run: run, specs: make(chan sandbox.LaunchSpec, 8)}
}

func (f *fakeLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	f.specs <- spec
	return f.run(ctx, spec)
}

// blocking returns a launcher that waits for ctx and reports its error.
func blocking() *fakeLauncher {
	return newFakeLauncher(func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		<-ctx.Done()
		return sandbox.Result{ExitCode: -1}, ctx.Err()
	})
}

type fixture struct {
	t        *testing.T
	svc      *Service
	launcher *fakeLauncher
	torn     chan string
	root     string
}

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// operator is the caller every fixture run is made as unless a test says
// otherwise.
var operator = Caller{Name: "operator", Operator: true}

// fakeCheckouts is a CheckoutResolver over a fixed set of records.
type fakeCheckouts map[string]checkout.Record

func (f fakeCheckouts) Get(id string) (checkout.Record, error) {
	rec, ok := f[id]
	if !ok {
		return checkout.Record{}, checkout.ErrNotFound
	}
	return rec, nil
}

func newFixture(t *testing.T, launcher *fakeLauncher) *fixture {
	t.Helper()
	workDir := t.TempDir()
	store, err := runstate.Open(workDir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, launcher: launcher, torn: make(chan string, 8), root: root}
	backend := &Backend{
		Name:         "fake",
		Launcher:     launcher,
		Capabilities: Capabilities{NetworkNone: true},
		Teardown:     func(_ context.Context, name string) error { f.torn <- name; return nil },
	}
	cfg := Config{
		Image:          "img:1",
		RepoAllowlist:  []string{"https://github.com/hivecommons/", "https://git.example/org"},
		WorkspaceRoots: []string{root},
		MaxTimeoutMs:   time.Minute.Milliseconds(),
	}
	f.svc = New(store, backend, cfg)
	f.svc.Environ = func() []string { return []string{"KEEP=yes", "GITHUB_TOKEN=secret", "DROP=no"} }
	return f
}

func (f *fixture) workspace() string { return filepath.Join(f.root, "proj") }

func (f *fixture) start(req api.ContainedRunRequest) api.ContainedRun {
	f.t.Helper()
	run, created, err := f.svc.Create(context.Background(), operator, req)
	if err != nil || !created {
		f.t.Fatalf("Create: %v created=%v", err, created)
	}
	if run.Status != api.RunAccepted || run.ID == "" {
		f.t.Fatalf("accepted record = %+v", run)
	}
	return run
}

func (f *fixture) wait(id string) api.ContainedRun {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run, err := f.svc.Wait(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

func (f *fixture) spec() sandbox.LaunchSpec {
	f.t.Helper()
	select {
	case s := <-f.launcher.specs:
		return s
	case <-time.After(10 * time.Second):
		f.t.Fatal("launcher was never called")
	}
	return sandbox.LaunchSpec{}
}

func TestCompletedRunCarriesOutputAndSanitizedEnv(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "hello\n", Stderr: "warn\n"}, nil
	}))
	run := f.start(api.ContainedRunRequest{
		WorkspacePath: f.workspace(), Command: []string{"echo", "hello"},
		EnvAllowlist: []string{"KEEP", "MISSING"},
	})
	spec := f.spec()
	if spec.Image != "img:1" || spec.Name != "crux-run-"+run.ID || spec.Workspace != f.workspace() || spec.NetworkMode != "none" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Env["KEEP"] != "yes" || len(spec.Env) != 1 {
		t.Fatalf("env = %v", spec.Env)
	}
	done := f.wait(run.ID)
	if done.Status != api.RunCompleted || done.ExitCode == nil || *done.ExitCode != 0 || done.Stdout != "hello\n" || done.Stderr != "warn\n" {
		t.Fatalf("completed = %+v", done)
	}
	if done.Profile != api.RunProfileExperiment || done.Network != api.RunNetworkNone || done.Backend != "fake" || done.Workdir != f.workspace() {
		t.Fatalf("plan not recorded: %+v", done)
	}
	if done.StartedAt == nil || done.FinishedAt == nil || done.Owner != "operator" {
		t.Fatalf("timestamps/owner = %+v", done)
	}
	if out, _ := os.ReadFile(filepath.Join(f.svc.Store.Path(run.ID), StdoutArtifact)); string(out) != "hello\n" {
		t.Fatalf("stdout artifact = %q", out)
	}
	if got, err := f.svc.Get(run.ID); err != nil || got.Status != api.RunCompleted {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if len(f.torn) != 0 {
		t.Fatal("teardown ran for a completed run")
	}
}

func TestNonZeroExitIsCompletedWithTheCode(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stderr: "boom", ExitCode: 3}, errors.New("podman: exit status 3")
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"false"}}).ID)
	if run.Status != api.RunCompleted || run.ExitCode == nil || *run.ExitCode != 3 || run.Reason != "podman: exit status 3" {
		t.Fatalf("non-zero = %+v", run)
	}
}

func TestLauncherRefusalIsFailedWithoutAnExitCode(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, errors.New("image pull failed")
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunFailed || run.ExitCode != nil || run.Reason != "image pull failed" {
		t.Fatalf("refusal = %+v", run)
	}
}

func TestUnknownExitIsUnfinishedNeverGuessed(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "partial", ExitCode: -1}, errors.New("pod vanished")
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunUnfinished || run.ExitCode != nil || run.Stdout != "partial" || run.Reason != "pod vanished" {
		t.Fatalf("unknown exit = %+v", run)
	}
}

func TestTimeoutFailsAndTearsDown(t *testing.T) {
	f := newFixture(t, blocking())
	run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep"}, TimeoutMs: 20})
	done := f.wait(run.ID)
	if done.Status != api.RunFailed || !done.TimedOut || done.ExitCode != nil || done.Reason != "timed out" {
		t.Fatalf("timeout = %+v", done)
	}
	if name := <-f.torn; name != "crux-run-"+run.ID {
		t.Fatalf("teardown name = %q", name)
	}
}

func TestCancelTearsDownAndLateCancelReportsAlreadyFinished(t *testing.T) {
	f := newFixture(t, blocking())
	run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep"}})
	f.spec()
	f.svc.Backend.Teardown = func(context.Context, string) error { return errors.New("rm: token=abc123 gone") }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cancelled, err := f.svc.Cancel(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != api.RunCancelled || cancelled.AlreadyFinished || cancelled.ExitCode != nil {
		t.Fatalf("cancel = %+v", cancelled)
	}
	if !strings.HasPrefix(cancelled.Reason, "cancelled by the caller; teardown: ") {
		t.Fatalf("teardown error not recorded: %q", cancelled.Reason)
	}
	late, err := f.svc.Cancel(ctx, run.ID)
	if err != nil || !late.AlreadyFinished || late.Status != api.RunCancelled {
		t.Fatalf("late cancel = %+v, %v", late, err)
	}
	if _, err := f.svc.Cancel(ctx, "run_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown = %v", err)
	}
	if _, err := f.svc.Get("run_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown = %v", err)
	}
}

func TestCancelWithoutAGoroutineReportsTheRecord(t *testing.T) {
	f := newFixture(t, blocking())
	rec, _, err := f.svc.Store.Create(runstate.Record{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.svc.Cancel(context.Background(), rec.ID)
	if err != nil || run.Status != api.RunAccepted || run.AlreadyFinished {
		t.Fatalf("orphan cancel = %+v, %v", run, err)
	}
	changed, err := f.svc.Reconcile()
	if err != nil || len(changed) != 1 || changed[0].Status != runstate.StatusUnfinished || changed[0].Reason != ReconcileReason {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
}

func TestWaitHonoursItsContext(t *testing.T) {
	f := newFixture(t, blocking())
	run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.svc.Wait(ctx, run.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v", err)
	}
	f.svc.Cancel(context.Background(), run.ID)
}

func TestOutputIsScrubbedAndTruncatedWithTheOffsetRecorded(t *testing.T) {
	long := strings.Repeat("a", 50) + "secret" + strings.Repeat("b", 50)
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: long, Stderr: "short"}, nil
	}))
	f.svc.Scrub = func(v string) string { return strings.ReplaceAll(v, "secret", "[x]") }
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}, MaxOutputBytes: 10}).ID)
	if run.Stdout != strings.Repeat("a", 10) || run.StdoutTruncatedAt == nil || *run.StdoutTruncatedAt != 10 {
		t.Fatalf("truncated stdout = %q at %v", run.Stdout, run.StdoutTruncatedAt)
	}
	if run.Stderr != "short" || run.StderrTruncatedAt != nil {
		t.Fatalf("stderr = %q at %v", run.Stderr, run.StderrTruncatedAt)
	}
	full, err := os.ReadFile(filepath.Join(f.svc.Store.Path(run.ID), StdoutArtifact))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.ReplaceAll(long, "secret", "[x]"); string(full) != want {
		t.Fatalf("artifact = %q", full)
	}
}

func TestDefaultScrubRedactsCredentials(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok"}, nil
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if strings.Contains(run.Stdout, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("token survived scrubbing: %q", run.Stdout)
	}
}

func TestIdempotencyKeyReturnsTheExistingRun(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "once"}, nil
	}))
	req := api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}, IdempotencyKey: "k1"}
	first := f.wait(f.start(req).ID)
	again, created, err := f.svc.Create(context.Background(), operator, req)
	if err != nil || created || again.ID != first.ID || again.Status != api.RunCompleted || again.Stdout != "once" {
		t.Fatalf("replay = %+v created=%v err=%v", again, created, err)
	}
	other, created, err := f.svc.Create(context.Background(), Caller{Name: "someone-else", Operator: true}, req)
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("other owner shared the record: %+v created=%v err=%v", other, created, err)
	}
	f.wait(other.ID)
	if len(f.launcher.specs) != 2 {
		t.Fatalf("launches = %d, want 2", len(f.launcher.specs))
	}
}

func TestWorktreeIsProvisionedBeforeLaunch(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(_ context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	var got api.ContainedRunWorktree
	f.svc.Provisioner = provisionFunc(func(_ context.Context, dir string, wt api.ContainedRunWorktree) (string, error) {
		got = wt
		return filepath.Join(dir, "workspace"), nil
	})
	wt := &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}
	run := f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}})
	spec := f.spec()
	done := f.wait(run.ID)
	if got != *wt || spec.Workspace != filepath.Join(f.svc.Store.Path(run.ID), "workspace") || done.Workdir != spec.Workspace {
		t.Fatalf("worktree = %+v spec.Workspace=%q done=%+v", got, spec.Workspace, done)
	}
	if done.Status != api.RunCompleted {
		t.Fatalf("status = %s", done.Status)
	}
}

func TestCheckoutResolvesToItsWorktreeAndIsInUseWhileRunning(t *testing.T) {
	f := newFixture(t, blocking())
	path := filepath.Join(f.root, "proj")
	f.svc.Checkouts = fakeCheckouts{
		"chk_atlas":  {ID: "chk_atlas", Owner: "atlas", Path: path},
		"chk_beacon": {ID: "chk_beacon", Owner: "beacon", Path: path},
	}
	atlas := Caller{Name: "atlas"}
	cases := []struct {
		name   string
		caller Caller
		id     string
		want   any
		msg    string
	}{
		{"unknown id", atlas, "chk_nope", &RequestError{}, `checkout "chk_nope" not found`},
		{"another project's checkout", atlas, "chk_beacon", &ForbiddenError{}, `owned by "beacon", not by "atlas"`},
		{"checkout and workspace", atlas, "chk_atlas", &RequestError{}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := api.ContainedRunRequest{Command: []string{"x"}, Checkout: tc.id}
			if tc.name == "checkout and workspace" {
				req.WorkspacePath = path
			}
			_, created, err := f.svc.Create(context.Background(), tc.caller, req)
			if created || err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("Create: created=%v err=%v, want %q", created, err, tc.msg)
			}
			var reqErr *RequestError
			var forbidden *ForbiddenError
			if _, isReq := tc.want.(*RequestError); isReq != errors.As(err, &reqErr) || !isReq != errors.As(err, &forbidden) {
				t.Fatalf("err = %T, want %T", err, tc.want)
			}
		})
	}
	if len(f.launcher.specs) != 0 {
		t.Fatal("a refused request reached the launcher")
	}

	// The owner and the operator may both run in it; while either run is
	// live the checkout is in use, and afterwards it is not.
	var ids []string
	for _, caller := range []Caller{atlas, operator} {
		run, created, err := f.svc.Create(context.Background(), caller, api.ContainedRunRequest{Command: []string{"make"}, Checkout: "chk_atlas"})
		if err != nil || !created || run.Owner != caller.Name || run.Checkout != "chk_atlas" {
			t.Fatalf("Create as %s: %+v created=%v err=%v", caller.Name, run, created, err)
		}
		if spec := f.spec(); spec.Workspace != path {
			t.Fatalf("spec.Workspace = %q, want %q", spec.Workspace, path)
		}
		ids = append(ids, run.ID)
	}
	if !f.svc.InUse("chk_atlas") || f.svc.InUse("chk_beacon") {
		t.Fatal("InUse should name only the checkout with live runs")
	}
	for _, id := range ids {
		if done, err := f.svc.Cancel(context.Background(), id); err != nil || done.Status != api.RunCancelled || done.Checkout != "chk_atlas" || done.Workdir != path {
			t.Fatalf("Cancel %s: %+v err=%v", id, done, err)
		}
	}
	if f.svc.InUse("chk_atlas") {
		t.Fatal("InUse after both runs settled")
	}
}

func TestProvisionFailureIsFailedOrCancelled(t *testing.T) {
	f := newFixture(t, blocking())
	f.svc.Provisioner = provisionFunc(func(context.Context, string, api.ContainedRunWorktree) (string, error) {
		return "", errors.New("clone refused")
	})
	wt := &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}
	run := f.wait(f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}}).ID)
	if run.Status != api.RunFailed || run.Reason != "provisioning the worktree: clone refused" {
		t.Fatalf("failed provision = %+v", run)
	}

	f.svc.Provisioner = provisionFunc(func(ctx context.Context, _ string, _ api.ContainedRunWorktree) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	run = f.start(api.ContainedRunRequest{Worktree: wt, Command: []string{"make"}})
	cancelled, err := f.svc.Cancel(context.Background(), run.ID)
	if err != nil || cancelled.Status != api.RunCancelled || cancelled.Reason != "cancelled while provisioning the worktree" {
		t.Fatalf("cancelled provision = %+v, %v", cancelled, err)
	}
	if len(f.launcher.specs) != 0 {
		t.Fatal("launcher ran without a workspace")
	}
}

type provisionFunc func(ctx context.Context, dir string, wt api.ContainedRunWorktree) (string, error)

func (p provisionFunc) Provision(ctx context.Context, dir string, wt api.ContainedRunWorktree) (string, error) {
	return p(ctx, dir, wt)
}

func TestValidationErrors(t *testing.T) {
	f := newFixture(t, blocking())
	ws := f.workspace()
	wt := &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/hive", Commit: testSHA}
	cases := []struct {
		name string
		req  api.ContainedRunRequest
		want any
		msg  string
	}{
		{"no command", api.ContainedRunRequest{WorkspacePath: ws}, &RequestError{}, "command is required"},
		{"blank command", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{" "}}, &RequestError{}, "command is required"},
		{"agent profile", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Profile: "agent"}, &UnsupportedError{}, "S6"},
		{"bad profile", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Profile: "root"}, &RequestError{}, "profile must be"},
		{"bad network", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Network: "host"}, &RequestError{}, "network must be"},
		{"restricted unsupported", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, Network: "restricted"}, &UnsupportedError{}, `cannot enforce network "restricted"`},
		{"credential env", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, EnvAllowlist: []string{"GITHUB_TOKEN"}}, &RequestError{}, "credential"},
		{"timeout cap", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, TimeoutMs: time.Hour.Milliseconds()}, &RequestError{}, "timeout_ms"},
		{"output cap", api.ContainedRunRequest{WorkspacePath: ws, Command: []string{"x"}, MaxOutputBytes: DefaultMaxOutputCap + 1}, &RequestError{}, "max_output_bytes"},
		{"no source", api.ContainedRunRequest{Command: []string{"x"}}, &RequestError{}, "one of checkout, worktree or workspace_path"},
		{"two sources", api.ContainedRunRequest{Command: []string{"x"}, WorkspacePath: ws, Worktree: wt}, &RequestError{}, "mutually exclusive"},
		{"checkout without a service", api.ContainedRunRequest{Command: []string{"x"}, Checkout: "chk_1"}, &UnsupportedError{}, "this server has none"},
		{"worktree repo", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Commit: testSHA}}, &RequestError{}, "worktree.repo"},
		{"worktree short sha", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: wt.Repo, Commit: "main"}}, &RequestError{}, "40-hex"},
		{"worktree forbidden", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://example.com/r", Commit: testSHA}}, &ForbiddenError{}, "repo_allowlist"},
		// The allowlist is a string prefix; git resolves ".." (libcurl for
		// https, literally for file) and decodes percent-encoding, so a
		// non-canonical URL must be refused before the prefix is compared.
		{"worktree dot segments", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/../octocat/Hello-World", Commit: testSHA}}, &RequestError{}, "canonical"},
		{"worktree percent-encoded", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://github.com/hivecommons/%2e%2e/octocat/Hello-World", Commit: testSHA}}, &RequestError{}, "canonical"},
		{"worktree credentials", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://user:tok@github.com/hivecommons/hive", Commit: testSHA}}, &RequestError{}, "credentials"},
		// An entry without a trailing slash is a repository or org name, not
		// a string prefix: it must not admit a sibling or a longer host.
		{"worktree sibling prefix", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://git.example/org-evil/repo", Commit: testSHA}}, &ForbiddenError{}, "repo_allowlist"},
		{"worktree host suffix", api.ContainedRunRequest{Command: []string{"x"}, Worktree: &api.ContainedRunWorktree{Repo: "https://git.example.evil/org/repo", Commit: testSHA}}, &ForbiddenError{}, "repo_allowlist"},
		{"relative workspace", api.ContainedRunRequest{Command: []string{"x"}, WorkspacePath: "proj"}, &RequestError{}, "absolute"},
		{"workspace outside roots", api.ContainedRunRequest{Command: []string{"x"}, WorkspacePath: filepath.Dir(f.root)}, &ForbiddenError{}, "workspace_roots"},
		{"workspace sibling prefix", api.ContainedRunRequest{Command: []string{"x"}, WorkspacePath: f.root + "2"}, &ForbiddenError{}, "workspace_roots"},
		{"workspace missing", api.ContainedRunRequest{Command: []string{"x"}, WorkspacePath: filepath.Join(ws, "nope")}, &RequestError{}, "not a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, created, err := f.svc.Create(context.Background(), operator, tc.req)
			if created || err == nil {
				t.Fatalf("Create accepted: created=%v err=%v", created, err)
			}
			var ok bool
			switch tc.want.(type) {
			case *RequestError:
				var e *RequestError
				ok = errors.As(err, &e)
			case *ForbiddenError:
				var e *ForbiddenError
				ok = errors.As(err, &e)
			case *UnsupportedError:
				var e *UnsupportedError
				ok = errors.As(err, &e)
			}
			if !ok || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %T %v, want %T containing %q", err, err, tc.want, tc.msg)
			}
		})
	}
	if len(f.launcher.specs) != 0 {
		t.Fatal("a refused request reached the launcher")
	}
}

func TestNotConfiguredServesRecordsButRefusesCreates(t *testing.T) {
	store, err := runstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(store, nil, Config{})
	if svc.Configured() {
		t.Fatal("Configured without a backend")
	}
	if _, _, err := svc.Create(context.Background(), operator, api.ContainedRunRequest{Command: []string{"x"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Create = %v", err)
	}
	rec, _, _ := store.Create(runstate.Record{})
	if run, err := svc.Get(rec.ID); err != nil || run.ID != rec.ID {
		t.Fatalf("Get = %+v, %v", run, err)
	}
}

func TestClassifyTable(t *testing.T) {
	unknown := sandbox.Result{ExitCode: -1}
	boom := errors.New("boom")
	cases := []struct {
		name       string
		res        sandbox.Result
		err        error
		cancelled  bool
		timedOut   bool
		status     string
		wantExit   *int
		wantReason string
	}{
		{"ok", sandbox.Result{}, nil, false, false, runstate.StatusCompleted, ptr(0), ""},
		{"non-zero", sandbox.Result{ExitCode: 2}, boom, false, false, runstate.StatusCompleted, ptr(2), "boom"},
		{"refused", sandbox.Result{}, boom, false, false, runstate.StatusFailed, nil, "boom"},
		{"unknown", unknown, boom, false, false, runstate.StatusUnfinished, nil, "boom"},
		{"cancelled", unknown, context.Canceled, true, false, runstate.StatusCancelled, nil, "cancelled by the caller"},
		{"cancelled with exit", sandbox.Result{ExitCode: 137}, boom, true, false, runstate.StatusCancelled, ptr(137), "cancelled by the caller"},
		{"timed out", unknown, context.DeadlineExceeded, false, true, runstate.StatusFailed, nil, "timed out"},
	}
	for _, tc := range cases {
		status, reason, exit := classify(tc.res, tc.err, tc.cancelled, tc.timedOut)
		if status != tc.status || reason != tc.wantReason || (exit == nil) != (tc.wantExit == nil) || (exit != nil && *exit != *tc.wantExit) {
			t.Errorf("%s: got %s %q %v", tc.name, status, reason, exit)
		}
	}
}

func ptr(v int) *int { return &v }

func TestCommandLogRecordsWhatTheRunnerDispatched(t *testing.T) {
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	cases := []struct {
		name   string
		res    sandbox.Result
		err    error
		status string
		exit   *int
	}{
		{"completed", sandbox.Result{Stdout: "built " + token + "\n", Stderr: "warn\n"}, nil, api.CommandCompleted, new(0)},
		{"non-zero exit", sandbox.Result{Stderr: "boom", ExitCode: 3}, errors.New("exit status 3"), api.CommandCompleted, new(3)},
		{"refused", sandbox.Result{}, errors.New("image pull failed"), api.CommandFailed, nil},
		{"exit not knowable", sandbox.Result{ExitCode: -1}, errors.New("pod vanished"), api.CommandUnfinished, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) { return tc.res, tc.err }))
			run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"make", "-j2", "test"}}).ID)
			page, err := f.svc.CommandLog(run.ID, 0, 0)
			if err != nil || page.Run != run.ID || len(page.Commands) != 1 || page.More {
				t.Fatalf("CommandLog = %+v, %v", page, err)
			}
			rec := page.Commands[0]
			if strings.Join(rec.Command, " ") != "make -j2 test" || rec.Cwd != f.workspace() || rec.Capture != api.CaptureRunner || rec.Status != tc.status || rec.EndedAt == nil {
				t.Fatalf("record = %+v", rec)
			}
			if (rec.ExitCode == nil) != (tc.exit == nil) || (rec.ExitCode != nil && *rec.ExitCode != *tc.exit) {
				t.Fatalf("exit = %v, want %v", rec.ExitCode, tc.exit)
			}
			// The log counts the same scrubbed bytes the run keeps as its
			// artifact, and never the secret.
			full, err := os.ReadFile(filepath.Join(f.svc.Store.Path(run.ID), StdoutArtifact))
			if err != nil {
				t.Fatal(err)
			}
			if rec.Stdout == nil || rec.Stdout.Bytes != int64(len(full)) || strings.Contains(rec.Stdout.Head, token) {
				t.Fatalf("stdout = %+v, artifact %q", rec.Stdout, full)
			}
		})
	}
}

func TestCommandLogRecordsCancelAndTimeout(t *testing.T) {
	f := newFixture(t, blocking())
	run := f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep", "60"}})
	f.spec()
	// While the command runs the log already holds it, unfinished.
	page, err := f.svc.CommandLog(run.ID, 0, 0)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Status != api.CommandUnfinished || page.Commands[0].EndedAt != nil {
		t.Fatalf("live log = %+v, %v", page, err)
	}
	if _, err := f.svc.Cancel(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	page, err = f.svc.CommandLog(run.ID, page.NextSeq, 0)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Status != api.CommandCancelled || page.Commands[0].Reason != "cancelled by the caller" {
		t.Fatalf("cancelled log = %+v, %v", page, err)
	}

	timed := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"sleep"}, TimeoutMs: 20}).ID)
	page, err = f.svc.CommandLog(timed.ID, 0, 0)
	if err != nil || len(page.Commands) != 1 || page.Commands[0].Status != api.CommandFailed || page.Commands[0].Reason != "timed out" {
		t.Fatalf("timed-out log = %+v, %v", page, err)
	}
}

func TestCommandLogPolicyComesFromTheConfig(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{Stdout: "0123456789abcdef"}, nil
	}))
	f.svc = New(f.svc.Store, f.svc.Backend, Config{Image: "img", WorkspaceRoots: f.svc.Config.WorkspaceRoots, CmdlogHeadBytes: 4, CmdlogTailBytes: 2})
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	page, err := f.svc.CommandLog(run.ID, 0, 0)
	if err != nil || len(page.Commands) != 1 {
		t.Fatalf("CommandLog = %+v, %v", page, err)
	}
	if out := page.Commands[0].Stdout; out.Head != "0123" || out.Tail != "ef" || !out.Truncated || out.Bytes != 16 {
		t.Fatalf("stdout = %+v", out)
	}
}

func TestUnrecordableCommandIsNotRun(t *testing.T) {
	f := newFixture(t, blocking())
	f.svc.Store.NewID = func() string { return "run_unrecordable" }
	// The log's path is taken by a directory, so the started line cannot
	// be written.
	if err := os.MkdirAll(filepath.Join(f.svc.Store.Path("run_unrecordable"), cmdlog.FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunFailed || !strings.HasPrefix(run.Reason, "recording the command: ") || run.ExitCode != nil {
		t.Fatalf("unrecordable = %+v", run)
	}
	if len(f.launcher.specs) != 0 {
		t.Fatal("the launcher ran a command the log could not record")
	}
	if _, err := f.svc.CommandLog(run.ID, 0, 0); err == nil {
		t.Fatal("read a log that is a directory")
	}
}

func TestUnwritableEndIsNotedOnTheRun(t *testing.T) {
	var f *fixture
	f = newFixture(t, newFakeLauncher(func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		// The command replaces the log with a directory before it ends.
		path := filepath.Join(f.svc.Store.Path(strings.TrimPrefix(spec.Name, launchNamePrefix)), cmdlog.FileName)
		if err := os.Remove(path); err != nil {
			return sandbox.Result{}, err
		}
		return sandbox.Result{Stdout: "ok"}, os.Mkdir(path, 0o700)
	}))
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	if run.Status != api.RunCompleted || !strings.Contains(run.Reason, "; command log: ") {
		t.Fatalf("run = %+v", run)
	}
}

func TestCommandLogOfUnknownOrUnloggedRuns(t *testing.T) {
	f := newFixture(t, newFakeLauncher(func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
		return sandbox.Result{}, nil
	}))
	if _, err := f.svc.CommandLog("run_missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	f.svc.Commands = nil
	run := f.wait(f.start(api.ContainedRunRequest{WorkspacePath: f.workspace(), Command: []string{"x"}}).ID)
	page, err := f.svc.CommandLog(run.ID, 0, 0)
	if run.Status != api.RunCompleted || err != nil || len(page.Commands) != 0 || page.Run != run.ID {
		t.Fatalf("run %+v without a recorder: %+v, %v", run, page, err)
	}
}
