package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// fakeRuntime is a crux.Runtime that records the calls the CLI makes and
// answers from canned state. Methods the CLI does not use panic through the
// embedded nil interface, which is what we want: a subcommand reaching past
// its documented Runtime methods fails the test loudly.
type fakeRuntime struct {
	crux.Runtime

	mu        sync.Mutex
	calls     []string
	procs     map[string]*crux.Process
	buffer    map[string][][]string // successive GetBufferOutput answers
	pane      map[string][]string
	fullLog   map[string]string
	header    map[string]string
	startErr  error
	stopErr   error
	kickErr   error
	outputErr error
	kicks     []string
	// started is closed on the first Start so a test can sequence after it.
	started chan struct{}
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		procs:   map[string]*crux.Process{},
		buffer:  map[string][][]string{},
		pane:    map[string][]string{},
		fullLog: map[string]string{},
		header:  map[string]string{},
		started: make(chan struct{}),
	}
}

func (f *fakeRuntime) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeRuntime) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRuntime) Kicks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kicks...)
}

func (f *fakeRuntime) AddAgent(name string, cfg config.AgentConfig) {
	f.record("AddAgent(%s,%s)", name, cfg.Backend)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[name] = &crux.Process{Name: name, ID: cfg.ID, Config: cfg, State: crux.StateStopped}
}

func (f *fakeRuntime) Start(_ context.Context, name string) error {
	f.record("Start(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.started:
	default:
		close(f.started)
	}
	if f.startErr != nil {
		return f.startErr
	}
	p, ok := f.procs[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	p.State = crux.StateRunning
	now := time.Now()
	p.StartedAt = &now
	return nil
}

func (f *fakeRuntime) Stop(name string) error {
	f.record("Stop(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	p, ok := f.procs[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	p.State = crux.StateStopped
	return nil
}

func (f *fakeRuntime) AllStatuses() map[string]*crux.Process {
	f.record("AllStatuses()")
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*crux.Process, len(f.procs))
	for k, v := range f.procs {
		out[k] = v
	}
	return out
}

func (f *fakeRuntime) GetStatus(name string) (*crux.Process, error) {
	f.record("GetStatus(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.procs[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	return p, nil
}

func (f *fakeRuntime) ResolveAgent(nameOrID string) string {
	f.record("ResolveAgent(%s)", nameOrID)
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, p := range f.procs {
		if p.ID == nameOrID {
			return name
		}
	}
	return nameOrID
}

func (f *fakeRuntime) GetOutput(name string, lines int) ([]string, error) {
	f.record("GetOutput(%s,%d)", name, lines)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	return f.pane[name], nil
}

func (f *fakeRuntime) GetBufferOutput(name string, lines int) ([]string, error) {
	f.record("GetBufferOutput(%s,%d)", name, lines)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return nil, f.outputErr
	}
	answers := f.buffer[name]
	if len(answers) == 0 {
		return nil, nil
	}
	cur := answers[0]
	if len(answers) > 1 {
		f.buffer[name] = answers[1:]
	}
	return cur, nil
}

func (f *fakeRuntime) CaptureFullLog(name string) (string, error) {
	f.record("CaptureFullLog(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outputErr != nil {
		return "", f.outputErr
	}
	return f.fullLog[name], nil
}

func (f *fakeRuntime) FullLogHeader(name string) string {
	f.record("FullLogHeader(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.header[name]
}

func (f *fakeRuntime) SendKick(name, message string) error {
	f.record("SendKick(%s)", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kickErr != nil {
		return f.kickErr
	}
	if _, ok := f.procs[name]; !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	f.kicks = append(f.kicks, message)
	return nil
}

// SetKickObserver is what `crux serve` installs so the API can relay kick
// events; the fake only records that it was asked.
func (f *fakeRuntime) SetKickObserver(fn func(agentName, event, detail string)) {
	f.record("SetKickObserver(%t)", fn != nil)
}

// harness wires an Env to a fake runtime, captured streams and a hand-driven
// ticker.
type harness struct {
	t      *testing.T
	rt     *fakeRuntime
	env    *Env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ticks  chan time.Time
	// defs and project are what the factory was last handed.
	defs    map[string]config.AgentConfig
	project crux.ProjectContext
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, rt: newFakeRuntime(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, ticks: make(chan time.Time)}
	h.env = &Env{
		Stdin:  strings.NewReader(""),
		Stdout: h.stdout,
		Stderr: h.stderr,
		Loader: fakeLoader{},
		newRuntime: func(defs map[string]config.AgentConfig, project crux.ProjectContext) crux.Runtime {
			h.defs = defs
			h.project = project
			for name, cfg := range defs {
				h.rt.mu.Lock()
				h.rt.procs[name] = &crux.Process{Name: name, ID: cfg.ID, Config: cfg, State: crux.StateStopped}
				h.rt.mu.Unlock()
			}
			return h.rt
		},
		ticker: func(time.Duration) (<-chan time.Time, func()) { return h.ticks, func() {} },
	}
	return h
}

// workDirArgs returns the global flags that pin the CLI to a per-test work
// dir with no live socket, so tests never see each other or the user's home.
func (h *harness) workDirArgs() []string {
	return []string{"--workdir", h.t.TempDir()}
}

func (h *harness) run(args ...string) int {
	return h.env.Run(context.Background(), append(h.workDirArgs(), args...))
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const portableDefinition = `apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: Scout Agent
  displayName: Scout
spec:
  backend: claude
  model: claude-sonnet-4-6
  role: scout
  clearOnKick: true
`

const overlayDefinition = `backend: codex
model: gpt-5
role: fixer
`

func TestUsageAndFlagErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != ExitUsage {
		t.Fatalf("no command: code %d, stderr %q", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "usage: crux") {
		t.Fatalf("stderr = %q", h.stderr)
	}

	h = newHarness(t)
	if code := h.run("--help"); code != ExitOK {
		t.Fatalf("--help: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("help"); code != ExitOK {
		t.Fatalf("help: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("--no-such-flag", "ps"); code != ExitUsage {
		t.Fatalf("bad global flag: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("nope"); code != ExitUsage || !strings.Contains(h.stderr.String(), "unknown command") {
		t.Fatalf("unknown: code %d, stderr %q", code, h.stderr)
	}
}

func TestResolveWorkDirDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envWorkDir, "")
	var o globalOptions
	if err := o.resolve(); err != nil {
		t.Fatal(err)
	}
	if o.workDir != filepath.Join(home, defaultWorkDirName) {
		t.Fatalf("workDir = %q", o.workDir)
	}
	if o.socket != filepath.Join(o.workDir, controlSocketName) {
		t.Fatalf("socket = %q", o.socket)
	}
	if got := os.Getenv(envWorkDir); got != o.workDir {
		t.Fatalf("%s = %q, want %q", envWorkDir, got, o.workDir)
	}

	o = globalOptions{}
	t.Setenv(envWorkDir, filepath.Join(home, "fromenv"))
	if err := o.resolve(); err != nil {
		t.Fatal(err)
	}
	if o.workDir != filepath.Join(home, "fromenv") {
		t.Fatalf("workDir = %q, want env value", o.workDir)
	}
}

func TestRunFromPortableDefinitionStreamsAndStopsOnInterrupt(t *testing.T) {
	h := newHarness(t)
	def := writeFile(t, filepath.Join(t.TempDir(), "scout.yaml"), portableDefinition)
	h.rt.buffer["scout-agent"] = [][]string{
		{"boot", "ready"},
		{"boot", "ready", "working"},
		{"ready", "working", "done"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "run", "--no-socket", def))
	}()
	<-h.rt.started
	for i := 0; i < 3; i++ {
		h.ticks <- time.Time{}
	}
	cancel()
	code := <-done
	if code != ExitOK {
		t.Fatalf("code %d, stderr %q", code, h.stderr)
	}
	want := "started scout-agent\nboot\nready\nworking\ndone\nstopped scout-agent\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", h.stdout, want)
	}
	calls := h.rt.Calls()
	if calls[0] != "AddAgent(scout-agent,claude)" || calls[1] != "Start(scout-agent)" || calls[len(calls)-1] != "Stop(scout-agent)" {
		t.Fatalf("calls = %v", calls)
	}
	if cfg := h.rt.procs["scout-agent"].Config; cfg.Role != "scout" || cfg.ID != "scout-agent" || cfg.Model != "claude-sonnet-4-6" || !cfg.Enabled {
		t.Fatalf("definition not applied with defaults: %+v", cfg)
	}
}

func TestRunDetachReturnsAfterStart(t *testing.T) {
	h := newHarness(t)
	def := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	if code := h.run("run", "--detach", def); code != ExitOK {
		t.Fatalf("code %d, stderr %q", code, h.stderr)
	}
	if h.stdout.String() != "started fixer\n" {
		t.Fatalf("stdout = %q", h.stdout)
	}
	if calls := h.rt.Calls(); len(calls) != 2 || calls[0] != "AddAgent(fixer,codex)" || calls[1] != "Start(fixer)" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestRunErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run("run"); code != ExitUsage {
		t.Fatalf("no target: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("run", "--bogus", "x.yaml"); code != ExitUsage {
		t.Fatalf("bad flag: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("run", "-h"); code != ExitOK {
		t.Fatalf("-h: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("run", filepath.Join(t.TempDir(), "missing.yaml")); code != ExitError {
		t.Fatalf("missing file: code %d", code)
	}
	h = newHarness(t)
	if code := h.run("run", "not-in-config"); code != ExitError || !strings.Contains(h.stderr.String(), "not in --config") {
		t.Fatalf("name without config: code %d, stderr %q", code, h.stderr)
	}
	h = newHarness(t)
	if code := h.run("--config", filepath.Join(t.TempDir(), "nope.yaml"), "run", "x"); code != ExitError {
		t.Fatalf("bad config: code %d", code)
	}
	h = newHarness(t)
	h.rt.startErr = errors.New("tmux missing")
	def := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	if code := h.run("run", "--detach", def); code != ExitError || !strings.Contains(h.stderr.String(), "tmux missing") {
		t.Fatalf("start error: code %d, stderr %q", code, h.stderr)
	}
}

func TestRunStopErrorAndOutputError(t *testing.T) {
	h := newHarness(t)
	def := writeFile(t, filepath.Join(t.TempDir(), "fixer.yaml"), overlayDefinition)
	h.rt.outputErr = errors.New("pane gone")
	h.rt.stopErr = errors.New("refused")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- h.env.Run(ctx, append(h.workDirArgs(), "run", "--no-socket", def)) }()
	<-h.rt.started
	// The output error ends streaming without an interrupt.
	h.ticks <- time.Time{}
	if code := <-done; code != ExitError {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(h.stderr.String(), "pane gone") || !strings.Contains(h.stderr.String(), "refused") {
		t.Fatalf("stderr = %q", h.stderr)
	}
}

func TestNewTail(t *testing.T) {
	cases := []struct {
		prev, cur, want []string
	}{
		{nil, []string{"a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b"}, []string{"a", "b"}, nil},
		{[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{[]string{"a", "b", "c"}, []string{"b", "c", "d", "e"}, []string{"d", "e"}},
		{[]string{"a", "b"}, []string{"x", "y"}, []string{"x", "y"}},
		{[]string{"a", "b"}, []string{"b"}, nil},
	}
	for _, c := range cases {
		got := newTail(c.prev, c.cur)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("newTail(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func hiveYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return writeFile(t, filepath.Join(dir, "hive.yaml"), `
project:
  org: hivecommons
  repos: [hivecommons/hive]
github:
  token: test-token
data:
  agents_dir: `+filepath.Join(dir, "agent-configs")+`
agents:
  scout:
    backend: claude
    model: claude-sonnet-4-6
  sleeper:
    backend: codex
    enabled: false
`)
}

func TestPSTableAndJSONFromConfig(t *testing.T) {
	h := newHarness(t)
	cfg := hiveYAML(t)
	if code := h.run("--config", cfg, "ps"); code != ExitOK {
		t.Fatalf("code %d, stderr %q", code, h.stderr)
	}
	if _, ok := h.defs["scout"]; !ok || len(h.defs) != 1 {
		t.Fatalf("defs = %v, want only the enabled agent", h.defs)
	}
	if h.project.Org != "hivecommons" || h.project.PolicyDir != fakePolicyDir {
		t.Fatalf("project = %+v", h.project)
	}
	out := h.stdout.String()
	if !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "scout") || !strings.Contains(out, "claude") {
		t.Fatalf("table = %q", out)
	}

	h2 := newHarness(t)
	h2.rt.procs["zed"] = &crux.Process{Name: "zed", ID: "z", State: crux.StateRunning, Paused: true, ModelOverride: "m2", BackendOverride: "bob"}
	h2.rt.procs["nil"] = nil
	if code := h2.run("--local", "ps", "--json"); code != ExitOK {
		t.Fatalf("code %d, stderr %q", code, h2.stderr)
	}
	if !strings.Contains(h2.stderr.String(), "no --config") {
		t.Fatalf("expected the empty-runtime hint, got %q", h2.stderr)
	}
	js := h2.stdout.String()
	if !strings.Contains(js, `"name": "zed"`) || !strings.Contains(js, `"backend": "bob"`) || !strings.Contains(js, `"model": "m2"`) || !strings.Contains(js, `"paused": true`) {
		t.Fatalf("json = %s", js)
	}

	h3 := newHarness(t)
	h3.rt.procs["a"] = &crux.Process{Name: "a", ID: "a", State: crux.StateRunning, Paused: true}
	if code := h3.run("--local", "ps"); code != ExitOK || !strings.Contains(h3.stdout.String(), "(paused)") {
		t.Fatalf("paused table: code %d out %q", code, h3.stdout)
	}
	if code := newHarness(t).run("ps", "--bogus"); code != ExitUsage {
		t.Fatalf("bad flag: code %d", code)
	}
}

func TestLogsPaneBufferFullAndJSON(t *testing.T) {
	h := newHarness(t)
	h.rt.procs["scout"] = &crux.Process{Name: "scout", ID: "sc"}
	h.rt.pane["scout"] = []string{"p1", "p2"}
	h.rt.buffer["scout"] = [][]string{{"b1"}}
	h.rt.fullLog["scout"] = "all of it"
	h.rt.header["scout"] = "# header"

	if code := h.run("--local", "logs", "-n", "7", "sc"); code != ExitOK || h.stdout.String() != "p1\np2\n" {
		t.Fatalf("pane: code %d out %q err %q", code, h.stdout, h.stderr)
	}
	if calls := h.rt.Calls(); calls[len(calls)-1] != "GetOutput(scout,7)" {
		t.Fatalf("calls = %v", calls)
	}

	h.stdout.Reset()
	if code := h.run("--local", "logs", "--buffer", "scout"); code != ExitOK || h.stdout.String() != "b1\n" {
		t.Fatalf("buffer: code %d out %q", code, h.stdout)
	}

	h.stdout.Reset()
	if code := h.run("--local", "logs", "--full", "scout"); code != ExitOK || h.stdout.String() != "# header\nall of it\n" {
		t.Fatalf("full: code %d out %q", code, h.stdout)
	}

	h.stdout.Reset()
	if code := h.run("--local", "logs", "--full", "--json", "scout"); code != ExitOK || !strings.Contains(h.stdout.String(), `"content": "all of it"`) {
		t.Fatalf("full json: code %d out %q", code, h.stdout)
	}

	h.stdout.Reset()
	if code := h.run("--local", "logs", "--json", "scout"); code != ExitOK || !strings.Contains(h.stdout.String(), `"source": "pane"`) {
		t.Fatalf("json: code %d out %q", code, h.stdout)
	}

	if code := h.run("--local", "logs"); code != ExitUsage {
		t.Fatalf("no agent: code %d", code)
	}
	if code := h.run("--local", "logs", "--buffer", "--full", "scout"); code != ExitUsage {
		t.Fatalf("exclusive flags: code %d", code)
	}
	if code := h.run("--local", "logs", "-n", "-5", "scout"); code != ExitUsage {
		t.Fatalf("negative -n: code %d", code)
	}
	if code := h.run("--local", "logs", "--bogus"); code != ExitUsage {
		t.Fatalf("bad flag: code %d", code)
	}
	h.rt.outputErr = errors.New("no pane")
	if code := h.run("--local", "logs", "scout"); code != ExitError || !strings.Contains(h.stderr.String(), "no pane") {
		t.Fatalf("error: code %d stderr %q", code, h.stderr)
	}
}

func TestKick(t *testing.T) {
	h := newHarness(t)
	h.rt.procs["scout"] = &crux.Process{Name: "scout", ID: "sc"}
	if code := h.run("--local", "kick", "sc", "fix", "the", "build"); code != ExitOK {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}
	if len(h.rt.kicks) != 1 || h.rt.kicks[0] != "fix the build" || h.stdout.String() != "kicked scout\n" {
		t.Fatalf("kicks = %v out %q", h.rt.kicks, h.stdout)
	}

	h.env.Stdin = strings.NewReader("from stdin\n")
	if code := h.run("--local", "kick", "scout", "-"); code != ExitOK || h.rt.kicks[1] != "from stdin" {
		t.Fatalf("stdin kick: code %d kicks %v", code, h.rt.kicks)
	}

	h.env.Stdin = strings.NewReader("   \n")
	if code := h.run("--local", "kick", "scout", "-"); code != ExitUsage {
		t.Fatalf("empty: code %d", code)
	}
	if code := h.run("--local", "kick", "scout"); code != ExitUsage {
		t.Fatalf("no message: code %d", code)
	}
	if code := h.run("--local", "kick", "--bogus"); code != ExitUsage {
		t.Fatalf("bad flag: code %d", code)
	}
	h.env.Stdin = failingReader{}
	if code := h.run("--local", "kick", "scout", "-"); code != ExitError {
		t.Fatalf("stdin error: code %d", code)
	}
	h.rt.kickErr = errors.New("not running")
	if code := h.run("--local", "kick", "scout", "go"); code != ExitError || !strings.Contains(h.stderr.String(), "not running") {
		t.Fatalf("kick error: code %d stderr %q", code, h.stderr)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken stdin") }

func TestStop(t *testing.T) {
	h := newHarness(t)
	h.rt.procs["scout"] = &crux.Process{Name: "scout", ID: "sc", State: crux.StateRunning}
	if code := h.run("--local", "stop", "sc"); code != ExitOK || h.stdout.String() != "stopped scout\n" {
		t.Fatalf("code %d out %q stderr %q", code, h.stdout, h.stderr)
	}
	if h.rt.procs["scout"].State != crux.StateStopped {
		t.Fatal("agent not stopped")
	}
	if code := h.run("--local", "stop"); code != ExitUsage {
		t.Fatalf("no agent: code %d", code)
	}
	if code := h.run("--local", "stop", "--bogus"); code != ExitUsage {
		t.Fatalf("bad flag: code %d", code)
	}
	if code := h.run("--local", "stop", "ghost"); code != ExitError {
		t.Fatalf("unknown: code %d", code)
	}
}

func TestNewManagerRuntimeBuildsAManager(t *testing.T) {
	t.Setenv(envWorkDir, t.TempDir())
	rt := newManagerRuntime(map[string]config.AgentConfig{"scout": {Backend: "claude", ID: "scout"}}, crux.ProjectContext{})
	if rt == nil {
		t.Fatal("nil runtime")
	}
	if rt.ResolveAgent("scout") != "scout" {
		t.Fatal("manager did not load the definition")
	}
	if _, ok := rt.AllStatuses()["scout"]; !ok {
		t.Fatal("manager has no scout process")
	}
}

func TestDefaultsFillCollaborators(t *testing.T) {
	e := &Env{}
	e.defaults()
	if e.Stdin == nil || e.Stdout == nil || e.Stderr == nil || e.newRuntime == nil || e.ticker == nil || e.dial == nil {
		t.Fatal("defaults left a collaborator nil")
	}
	ticks, stop := e.ticker(time.Hour)
	stop()
	if ticks == nil {
		t.Fatal("nil ticker channel")
	}
}

func TestMainEntryPoint(t *testing.T) {
	var stderr bytes.Buffer
	if code := Main(context.Background(), []string{"--workdir", t.TempDir(), "help"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, nil); code != ExitOK {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "usage: crux") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLocalReportsConfigErrors(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--local", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "ps"); code != ExitError || !strings.Contains(h.stderr.String(), "reading config") {
		t.Fatalf("code %d stderr %q", code, h.stderr)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed stdout") }

func TestJSONAndTableWriteErrors(t *testing.T) {
	h := newHarness(t)
	h.rt.procs["a"] = &crux.Process{Name: "a", ID: "a"}
	h.env.Stdout = failingWriter{}
	if code := h.run("--local", "ps", "--json"); code != ExitError || !strings.Contains(h.stderr.String(), "closed stdout") {
		t.Fatalf("json: code %d stderr %q", code, h.stderr)
	}
	if code := h.run("--local", "ps"); code != ExitError {
		t.Fatalf("table: code %d", code)
	}
}

func TestVersionFlag(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--version"); code != ExitOK {
		t.Fatalf("--version: code %d, stderr %q", code, h.stderr)
	}
	if got := h.stdout.String(); got != "crux "+DefaultVersion+"\n" {
		t.Fatalf("stdout = %q", got)
	}
	h = newHarness(t)
	h.env.Version = "v1.2.3"
	if code := h.run("--version", "ps"); code != ExitOK || h.stdout.String() != "crux v1.2.3\n" {
		t.Fatalf("--version with a command: code %d, stdout %q", code, h.stdout)
	}
}

func TestDefsFlagAndEnvSelectTheDefinitionSource(t *testing.T) {
	cfg := writeFile(t, filepath.Join(t.TempDir(), "hive.yaml"), "agents:\n  fixer:\n    backend: codex\n")

	h := newHarness(t)
	if code := h.run("--local", "--defs", cfg, "ps"); code != ExitOK {
		t.Fatalf("--defs: code %d, stderr %q", code, h.stderr)
	}
	if _, ok := h.defs["fixer"]; !ok {
		t.Fatalf("--defs did not reach the loader: defs = %v", h.defs)
	}

	t.Setenv(DefsEnvVar, cfg)
	h = newHarness(t)
	if code := h.run("--local", "ps"); code != ExitOK {
		t.Fatalf("%s: code %d, stderr %q", DefsEnvVar, code, h.stderr)
	}
	if _, ok := h.defs["fixer"]; !ok {
		t.Fatalf("%s did not reach the loader: defs = %v", DefsEnvVar, h.defs)
	}

	// An explicit flag wins over the environment.
	h = newHarness(t)
	if code := h.run("--local", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "ps"); code != ExitError {
		t.Fatalf("flag over env: code %d, stderr %q", code, h.stderr)
	}
}

func TestSignalContextIsCancellable(t *testing.T) {
	ctx, stop := SignalContext()
	stop()
	<-ctx.Done()
}
