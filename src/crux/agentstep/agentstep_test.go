package agentstep

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/sandbox"
)

// fakeLauncher answers each launch from a queue of results, recording the
// spec and the prompt the backend would have read.
type fakeLauncher struct {
	mu      sync.Mutex
	specs   []sandbox.LaunchSpec
	prompts []string
	results []func(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error)
}

func (f *fakeLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	// The prompt path is the last word of the sh -lc line.
	line := spec.Command[len(spec.Command)-1]
	rel := strings.Trim(line[strings.LastIndex(line, " < ")+3:], "'")
	data, _ := os.ReadFile(filepath.Join(spec.Workspace, rel))
	f.prompts = append(f.prompts, string(data))
	next := f.results[0]
	if len(f.results) > 1 {
		f.results = f.results[1:]
	}
	f.mu.Unlock()
	return next(ctx, spec)
}

func answers(outs ...string) *fakeLauncher {
	f := &fakeLauncher{}
	for _, o := range outs {
		o := o
		f.results = append(f.results, func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{Stdout: o}, nil
		})
	}
	return f
}

type recorded struct {
	kind string
	data api.SignalStepData
}

type fixture struct {
	t        *testing.T
	host     Host
	launcher *fakeLauncher
	signals  []recorded
	torn     []string
	log      cmdlog.Recorder
}

func newFixture(t *testing.T, launcher *fakeLauncher) *fixture {
	t.Helper()
	f := &fixture{t: t, launcher: launcher}
	runDir := t.TempDir()
	log, err := cmdlog.FileOpener(cmdlog.Policy{})(runDir)
	if err != nil {
		t.Fatal(err)
	}
	f.log = log
	f.host = Host{
		Run: "run_1", RunDir: runDir, Workspace: t.TempDir(), Image: "img:1", Network: "restricted",
		Env: map[string]string{"KEEP": "yes"}, EnvAllowlist: []string{"KEEP"},
		Launcher: launcher, Timeout: time.Minute, MaxTimeout: time.Hour, CmdLog: log,
		Teardown: func(_ context.Context, name string) error { f.torn = append(f.torn, name); return nil },
		Signal: func(kind, _ string, data json.RawMessage) {
			var d api.SignalStepData
			_ = json.Unmarshal(data, &d)
			f.signals = append(f.signals, recorded{kind, d})
		},
	}
	return f
}

const okSchema = `{"type":"object","required":["verdict"],"properties":{"verdict":{"enum":["pass","fail"]}}}`

func (f *fixture) process(prompt string, schema string) api.StepRequest {
	return api.StepRequest{Backend: "process", Prompt: prompt, OutputSchema: json.RawMessage(schema), Command: []string{"./ask"}}
}

func (f *fixture) run(req api.StepRequest) api.Step {
	f.t.Helper()
	step, err := Run(context.Background(), f.host, req)
	if err != nil {
		f.t.Fatalf("Run: %v", err)
	}
	return step
}

func (f *fixture) commands() []api.CommandRecord {
	f.t.Helper()
	page, err := cmdlog.Read(f.host.RunDir, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return page.Commands
}

func TestSucceededStepIsRecordedLoggedAndSignalled(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	step := f.run(f.process("Is it fine?", okSchema))
	if step.N != 1 || step.Status != api.StepSucceeded || step.Attempts != 1 || string(step.Answer) != `{"verdict":"pass"}` || step.FinishedAt == nil || step.Backend != "process" {
		t.Fatalf("step = %+v", step)
	}
	spec := f.launcher.specs[0]
	if spec.Name != "crux-step-run_1-1-1" || spec.Image != "img:1" || spec.NetworkMode != "restricted" || spec.Env["KEEP"] != "yes" || spec.Workspace != f.host.Workspace {
		t.Fatalf("spec = %+v", spec)
	}
	if f.launcher.prompts[0] != "Is it fine?" {
		t.Fatalf("first prompt = %q, want the caller's verbatim", f.launcher.prompts[0])
	}
	got, err := Store{RunDir: f.host.RunDir}.Get(1)
	if err != nil || got.Status != api.StepSucceeded || got.TranscriptPath != "steps/1/transcript.jsonl" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	transcript, _ := os.ReadFile(filepath.Join(f.host.RunDir, got.TranscriptPath))
	if n := strings.Count(string(transcript), "\n"); n != 1 || !strings.Contains(string(transcript), `"status":"succeeded"`) {
		t.Fatalf("transcript = %s", transcript)
	}
	if cmds := f.commands(); len(cmds) != 1 || cmds[0].Status != api.CommandCompleted || cmds[0].Command[0] != "sh" {
		t.Fatalf("cmdlog = %+v", cmds)
	}
	if len(f.signals) != 1 || f.signals[0].kind != api.SignalStep || f.signals[0].data != (api.SignalStepData{Step: 1, Status: api.StepSucceeded, Attempts: 1}) {
		t.Fatalf("signals = %+v", f.signals)
	}
	if len(f.torn) != 0 {
		t.Fatal("a finished step was torn down")
	}
}

func TestMismatchRetriesWithTheViolationsThenGivesUp(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"maybe"}`, `prose`, `{"verdict":"pass"}`))
	req := f.process("Judge it.", okSchema)
	req.RetryOnMismatch = 2
	step := f.run(req)
	if step.Status != api.StepSucceeded || step.Attempts != 3 || step.N != 1 {
		t.Fatalf("step = %+v", step)
	}
	if p := f.launcher.prompts[1]; !strings.HasPrefix(p, "Judge it.\n\n") || !strings.Contains(p, "$.verdict") || !strings.Contains(p, "remaining after this one: 1") {
		t.Fatalf("second prompt = %q", p)
	}
	if p := f.launcher.prompts[2]; !strings.Contains(p, ErrNoAnswer.Error()) || !strings.Contains(p, "remaining after this one: 0") {
		t.Fatalf("third prompt = %q", p)
	}
	if len(f.commands()) != 3 || len(f.signals) != 1 {
		t.Fatalf("cmdlog %d signals %d", len(f.commands()), len(f.signals))
	}

	f = newFixture(t, answers(`{"verdict":"maybe"}`))
	req = f.process("Judge it.", okSchema)
	req.RetryOnMismatch = 1
	step = f.run(req)
	if step.Status != api.StepSchemaMismatch || step.Attempts != 2 || step.Answer != nil || len(step.Violations) != 1 || step.Violations[0].Path != "$.verdict" || !strings.Contains(step.Reason, "after 2 attempt") {
		t.Fatalf("step = %+v", step)
	}
	if f.signals[0].data.Status != api.StepSchemaMismatch {
		t.Fatalf("signal = %+v", f.signals)
	}
}

func TestStepsNumberUpAndListInOrder(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	for i := 1; i <= 3; i++ {
		if step := f.run(f.process("p", okSchema)); step.N != i {
			t.Fatalf("step %d numbered %d", i, step.N)
		}
	}
	store := Store{RunDir: f.host.RunDir}
	steps, err := store.List()
	if err != nil || len(steps) != 3 || steps[0].N != 1 || steps[2].N != 3 {
		t.Fatalf("List = %+v, %v", steps, err)
	}
	if _, err := store.Get(4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(4) = %v", err)
	}
	if _, err := store.Get(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(0) = %v", err)
	}
	// A corrupt record is reported after the readable ones.
	if err := os.WriteFile(filepath.Join(store.Dir(2), RecordName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	steps, err = store.List()
	if !errors.Is(err, ErrCorrupt) || len(steps) != 2 {
		t.Fatalf("List with a corrupt record = %+v, %v", steps, err)
	}
	// Numbering still moves past it.
	if step := f.run(f.process("p", okSchema)); step.N != 4 {
		t.Fatalf("after corrupt: N = %d", step.N)
	}
	if steps, err := (Store{RunDir: t.TempDir()}).List(); err != nil || len(steps) != 0 {
		t.Fatalf("empty List = %+v, %v", steps, err)
	}
}

func TestPromptPathReadsFromTheWorkspace(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	if err := os.WriteFile(filepath.Join(f.host.Workspace, "task.md"), []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := f.process("", okSchema)
	req.PromptPath = "task.md"
	f.run(req)
	if f.launcher.prompts[0] != "from file" {
		t.Fatalf("prompt = %q", f.launcher.prompts[0])
	}
}

func TestPromptPathOverTheBoundIsRefused(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	// A sparse file: the bound is on bytes present, not bytes on disk.
	if err := os.WriteFile(filepath.Join(f.host.Workspace, "task.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(f.host.Workspace, "task.md"), MaxPromptFileBytes+1); err != nil {
		t.Fatal(err)
	}
	req := f.process("", okSchema)
	req.PromptPath = "task.md"
	_, err := Run(context.Background(), f.host, req)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) || !strings.Contains(err.Error(), "over") {
		t.Fatalf("err = %v, want *RequestError naming the bound", err)
	}
	if len(f.launcher.prompts) != 0 {
		t.Fatalf("an oversize prompt file reached the step: %q", f.launcher.prompts)
	}
}

func TestOversizeAnswerFileIsNotReadIntoMemory(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n")
	launcher := &fakeLauncher{}
	launcher.results = append(launcher.results, func(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
		// The sandboxed agent writes the answer file codex was told to
		// produce (--output-last-message '<rel>'), here far over the bound.
		line := spec.Command[len(spec.Command)-1]
		rest := line[strings.Index(line, "--output-last-message '")+len("--output-last-message '"):]
		rel := rest[:strings.Index(rest, "'")]
		path := filepath.Join(spec.Workspace, filepath.FromSlash(rel))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return sandbox.Result{}, err
		}
		if err := os.Truncate(path, MaxPromptFileBytes+1); err != nil {
			return sandbox.Result{}, err
		}
		return sandbox.Result{Stdout: stdout}, nil
	})
	f := newFixture(t, launcher)
	step := f.run(api.StepRequest{Backend: "codex", Prompt: "judge", OutputSchema: json.RawMessage(okSchema)})
	// The file is refused and the last agent_message on stdout stands in.
	if step.Status != api.StepSucceeded || string(step.Answer) != `{"verdict":"pass"}` {
		t.Fatalf("step = %+v", step)
	}
}

func TestPromptPathSymlinkOutsideWorkspaceIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("host-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.host.Workspace, "task.md")); err != nil {
		t.Fatal(err)
	}
	req := f.process("", okSchema)
	req.PromptPath = "task.md"
	_, err := Run(context.Background(), f.host, req)
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("err = %v, want *RequestError", err)
	}
	if strings.Contains(err.Error(), "host-secret") || len(f.launcher.prompts) != 0 {
		t.Fatalf("outside file reached the step: %v, prompts %q", err, f.launcher.prompts)
	}
}

func TestSymlinkedCruxDirCannotRedirectThePromptWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	f := newFixture(t, answers(`{"verdict":"pass"}`))
	outside := t.TempDir()
	link := filepath.Join(f.host.Workspace, filepath.FromSlash(workspaceDir))
	if err := os.MkdirAll(filepath.Dir(link), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	step := Step(t, f)
	if step.Status != api.StepBackendError {
		t.Fatalf("status = %q, want %q", step.Status, api.StepBackendError)
	}
	var found []string
	_ = filepath.WalkDir(outside, func(p string, d os.DirEntry, _ error) error {
		if p != outside {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("wrote outside the workspace: %v", found)
	}
}

func Step(t *testing.T, f *fixture) api.Step {
	t.Helper()
	return f.run(f.process("hello", okSchema))
}

func TestRequestRefusals(t *testing.T) {
	f := newFixture(t, answers(`{}`))
	long := strings.Repeat("x", MaxPromptBytes+1)
	for _, tc := range []struct {
		name string
		req  api.StepRequest
		msg  string
	}{
		{"backend", api.StepRequest{Backend: "gemini", Prompt: "p", OutputSchema: json.RawMessage(okSchema)}, "unknown backend"},
		{"both prompts", api.StepRequest{Backend: "process", Prompt: "p", PromptPath: "x", OutputSchema: json.RawMessage(okSchema)}, "mutually exclusive"},
		{"no prompt", api.StepRequest{Backend: "process", OutputSchema: json.RawMessage(okSchema)}, "one of prompt or prompt_path"},
		{"blank prompt", api.StepRequest{Backend: "process", Prompt: " ", OutputSchema: json.RawMessage(okSchema)}, "prompt is empty"},
		{"long prompt", api.StepRequest{Backend: "process", Prompt: long, OutputSchema: json.RawMessage(okSchema)}, "bound"},
		{"absolute prompt path", api.StepRequest{Backend: "process", PromptPath: "/etc/passwd", OutputSchema: json.RawMessage(okSchema)}, "relative"},
		{"escaping prompt path", api.StepRequest{Backend: "process", PromptPath: "../x", OutputSchema: json.RawMessage(okSchema)}, "relative"},
		{"missing prompt path", api.StepRequest{Backend: "process", PromptPath: "nope.md", OutputSchema: json.RawMessage(okSchema)}, "nope.md"},
		{"no schema", api.StepRequest{Backend: "process", Prompt: "p"}, "output_schema"},
		{"bad schema", api.StepRequest{Backend: "process", Prompt: "p", OutputSchema: json.RawMessage(`{"oneOf":[]}`)}, "oneOf"},
		{"retries", api.StepRequest{Backend: "process", Prompt: "p", OutputSchema: json.RawMessage(okSchema), RetryOnMismatch: MaxRetries + 1}, "retry_on_mismatch"},
		{"turn limit", api.StepRequest{Backend: "process", Prompt: "p", OutputSchema: json.RawMessage(okSchema), TurnLimit: -1}, "turn_limit"},
		{"timeout cap", api.StepRequest{Backend: "process", Prompt: "p", OutputSchema: json.RawMessage(okSchema), TimeoutMs: (2 * time.Hour).Milliseconds()}, "timeout_ms"},
		{"empty tool", api.StepRequest{Backend: "claude", Prompt: "p", OutputSchema: json.RawMessage(okSchema), Tools: []string{" "}}, "empty name"},
		{"process without command", api.StepRequest{Backend: "process", Prompt: "p", OutputSchema: json.RawMessage(okSchema)}, "needs command"},
		{"claude with command", api.StepRequest{Backend: "claude", Prompt: "p", OutputSchema: json.RawMessage(okSchema), Command: []string{"x"}}, "builds its own command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reqErr *RequestError
			_, err := Run(context.Background(), f.host, tc.req)
			if !errors.As(err, &reqErr) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %T %v, want RequestError containing %q", err, err, tc.msg)
			}
		})
	}
	if len(f.launcher.specs) != 0 {
		t.Fatal("a refused request was launched")
	}
	if steps, _ := (Store{RunDir: f.host.RunDir}).List(); len(steps) != 0 {
		t.Fatalf("a refused request was recorded: %+v", steps)
	}
}

func TestUnsupportedRequestIsRecordedAndRefused(t *testing.T) {
	f := newFixture(t, answers(`{}`))
	req := f.process("p", okSchema)
	req.Tools = []string{"Bash"}
	step, err := Run(context.Background(), f.host, req)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "cannot restrict tools") {
		t.Fatalf("err = %v", err)
	}
	if step.N != 1 || step.Status != api.StepUnsupported || step.Reason != err.Error() || step.Attempts != 0 {
		t.Fatalf("step = %+v", step)
	}
	if got, err := (Store{RunDir: f.host.RunDir}).Get(1); err != nil || got.Status != api.StepUnsupported {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if len(f.launcher.specs) != 0 || len(f.signals) != 1 || f.signals[0].data.Status != api.StepUnsupported {
		t.Fatalf("launched %d, signals %+v", len(f.launcher.specs), f.signals)
	}
}

func TestBackendFailureStatuses(t *testing.T) {
	// A non-zero exit is a backend error, logged failed, no retry.
	f := newFixture(t, &fakeLauncher{results: []func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error){
		func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 2, Stderr: "ghp_0123456789abcdefXYZ boom"}, nil
		},
	}})
	req := f.process("p", okSchema)
	req.RetryOnMismatch = 3
	step := f.run(req)
	if step.Status != api.StepBackendError || step.Attempts != 1 || !strings.Contains(step.Reason, "exited 2") {
		t.Fatalf("step = %+v", step)
	}
	if cmds := f.commands(); len(cmds) != 1 || cmds[0].Status != api.CommandFailed || *cmds[0].ExitCode != 2 {
		t.Fatalf("cmdlog = %+v", cmds)
	}
	transcript, _ := os.ReadFile(filepath.Join(f.host.RunDir, step.TranscriptPath))
	if strings.Contains(string(transcript), "ghp_0123456789abcdefXYZ") {
		t.Fatalf("transcript kept a secret: %s", transcript)
	}

	// A launcher that could not start anything is a backend error too.
	f = newFixture(t, &fakeLauncher{results: []func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error){
		func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("podman: image missing")
		},
	}})
	step = f.run(f.process("p", okSchema))
	if step.Status != api.StepBackendError || !strings.Contains(step.Reason, "image missing") {
		t.Fatalf("step = %+v", step)
	}
	if cmds := f.commands(); len(cmds) != 1 || cmds[0].Status != api.CommandFailed || cmds[0].ExitCode != nil {
		t.Fatalf("cmdlog = %+v", cmds)
	}
}

func TestTimeoutAndCancelTearDownTheLaunch(t *testing.T) {
	wait := func(ctx context.Context, _ sandbox.LaunchSpec) (sandbox.Result, error) {
		<-ctx.Done()
		return sandbox.Result{ExitCode: -1}, ctx.Err()
	}
	f := newFixture(t, &fakeLauncher{results: []func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error){wait}})
	req := f.process("p", okSchema)
	req.TimeoutMs = 1
	step := f.run(req)
	if step.Status != api.StepTimeout || !strings.Contains(step.Reason, "did not finish within 1ms") {
		t.Fatalf("step = %+v", step)
	}
	if len(f.torn) != 1 || f.torn[0] != "crux-step-run_1-1-1" {
		t.Fatalf("torn = %v", f.torn)
	}
	if cmds := f.commands(); len(cmds) != 1 || cmds[0].Status != api.CommandFailed {
		t.Fatalf("cmdlog = %+v", cmds)
	}

	f = newFixture(t, &fakeLauncher{results: []func(context.Context, sandbox.LaunchSpec) (sandbox.Result, error){wait}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	step, err := Run(ctx, f.host, f.process("p", okSchema))
	if err != nil || step.Status != api.StepCancelled || len(f.torn) != 1 {
		t.Fatalf("step = %+v, %v, torn %v", step, err, f.torn)
	}
	if cmds := f.commands(); len(cmds) != 1 || cmds[0].Status != api.CommandCancelled {
		t.Fatalf("cmdlog = %+v", cmds)
	}
	if f.signals[0].data.Status != api.StepCancelled {
		t.Fatalf("signals = %+v", f.signals)
	}
}

func TestHostWithoutLogOrSignalsStillRuns(t *testing.T) {
	f := newFixture(t, answers(`{"verdict":"fail"}`))
	f.host.CmdLog, f.host.Signal, f.host.Teardown = nil, nil, nil
	step := f.run(f.process("p", okSchema))
	if step.Status != api.StepSucceeded || len(f.signals) != 0 {
		t.Fatalf("step = %+v", step)
	}
}

func TestReconcileMarksRunningStepsUnfinished(t *testing.T) {
	store := Store{RunDir: t.TempDir()}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, st := range []string{api.StepSucceeded, api.StepRunning, api.StepRunning} {
		if _, err := store.Create(api.Step{Run: "run_1", Status: st, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// A corrupt record is skipped so the rest still settle.
	if err := os.MkdirAll(store.Dir(4), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(4), RecordName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := store.Reconcile("serve restarted", now)
	if err != nil || len(changed) != 2 || changed[0].N != 2 || changed[1].N != 3 {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
	for _, n := range []int{2, 3} {
		got, err := store.Get(n)
		if err != nil || got.Status != api.StepUnfinished || got.Reason != "serve restarted" || got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
			t.Fatalf("step %d = %+v, %v", n, got, err)
		}
	}
	if got, _ := store.Get(1); got.Status != api.StepSucceeded {
		t.Fatalf("finished step touched: %+v", got)
	}
	if again, err := store.Reconcile("x", now); err != nil || len(again) != 0 {
		t.Fatalf("second reconcile = %+v, %v", again, err)
	}
}
