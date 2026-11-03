package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/pkg/config"
)

// fakeJobPodLogReader scripts the pod observation path: a (possibly absent)
// latest pod, a sequence of PodStatus answers, and a log whose delivery can
// be gated so a followed stream is observed mid-flight without sleeping.
type fakeJobPodLogReader struct {
	mu          sync.Mutex
	pod         kubejob.PodInfo
	found       bool
	listErr     error
	statuses    []kubejob.PodInfo
	statusErr   error
	statusCalls int
	logLines    []string
	logErr      error
	followed    []bool
	launchNames []string
	// gate, when non-nil, is received from before the last log line is
	// written, so the test controls when the "live" stream ends.
	gate chan struct{}
	// lineWritten is signalled after each log line reaches the writer.
	lineWritten chan struct{}
}

func (f *fakeJobPodLogReader) LatestPod(_ context.Context, launchName string) (kubejob.PodInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchNames = append(f.launchNames, launchName)
	return f.pod, f.found, f.listErr
}

func (f *fakeJobPodLogReader) PodStatus(context.Context, string) (kubejob.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return kubejob.PodInfo{}, f.statusErr
	}
	i := f.statusCalls
	f.statusCalls++
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	if i < 0 {
		return f.pod, nil
	}
	return f.statuses[i], nil
}

func (f *fakeJobPodLogReader) StreamLog(ctx context.Context, _ string, follow bool, w io.Writer) error {
	f.mu.Lock()
	f.followed = append(f.followed, follow)
	lines, gate, logErr, written := f.logLines, f.gate, f.logErr, f.lineWritten
	f.mu.Unlock()
	for i, l := range lines {
		if gate != nil && i == len(lines)-1 {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, err := io.WriteString(w, l); err != nil {
			return err
		}
		if written != nil {
			written <- struct{}{}
		}
	}
	return logErr
}

// jobTerminalServer is apiServer with a sandboxed job-runtime agent "deps"
// alongside the stock tmux agent "scanner", plus a podman-runtime "lint".
func jobTerminalServer(t *testing.T, reader *fakeJobPodLogReader) (*Server, string) {
	t.Helper()
	s, deps := apiServer(t)
	on := true
	deps.Config.AgentSandbox = config.AgentSandboxConfig{Enabled: true, Image: "img"}
	deps.Config.Agents["deps"] = config.AgentConfig{Backend: "claude", Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: config.SandboxRuntimeJob}}
	deps.Config.Agents["lint"] = config.AgentConfig{Backend: "claude", Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &on}}
	root := t.TempDir()
	s.jobTerminal = jobTerminalDeps{reader: reader, workspaceRoot: root, pollInterval: time.Millisecond}
	return s, root
}

func TestJobTerminalRoutesAreUnderTerminalPrefix(t *testing.T) {
	// The handoff code and assertion cookie are keyed on /terminal; a route
	// elsewhere would silently lose terminal auth.
	for _, p := range []string{jobTerminalPagePath, jobTerminalStreamPath} {
		if !isTerminalPath(strings.ReplaceAll(p, "{name}", "deps")) {
			t.Fatalf("%s is not a terminal path", p)
		}
	}
}

func TestJobTerminalRejectsNonJobAgents(t *testing.T) {
	s, _ := jobTerminalServer(t, &fakeJobPodLogReader{})
	cases := map[string]int{
		"/terminal/job/nobody":         http.StatusNotFound,
		"/terminal/job/scanner":        http.StatusConflict, // not sandboxed
		"/terminal/job/lint":           http.StatusConflict, // podman runtime
		"/terminal/job/nobody/stream":  http.StatusNotFound,
		"/terminal/job/scanner/stream": http.StatusConflict,
		"/terminal/job/lint/stream":    http.StatusConflict,
	}
	for path, want := range cases {
		rec := doGet(s, path)
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d (%s)", path, rec.Code, want, rec.Body.String())
		}
	}
	rec := doGet(s, "/terminal/job/scanner")
	if !strings.Contains(rec.Body.String(), "/terminal/?arg=hive-scanner") {
		t.Fatalf("a tmux agent's 409 must point at its tmux terminal: %s", rec.Body.String())
	}
	rec = doGet(s, "/terminal/job/lint")
	if !strings.Contains(rec.Body.String(), `runtime "podman"`) {
		t.Fatalf("a podman agent's 409 must name the runtime: %s", rec.Body.String())
	}
}

func TestJobTerminalPageServesViewer(t *testing.T) {
	s, _ := jobTerminalServer(t, &fakeJobPodLogReader{})
	rec := doGet(s, "/terminal/job/deps")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"deps — sandbox job log", "'./deps/stream'", "read-only", `id="reconnect"`, "addEventListener('click'"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, " onclick=") || strings.Contains(body, " onload=") {
		t.Fatal("inline handler attributes are closed by CSP script-src-attr elsewhere; keep the viewer consistent")
	}
	// Agent names are escaped into the document.
	on := true
	s.deps.Config.Agents[`x<img src=x>`] = config.AgentConfig{Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: config.SandboxRuntimeJob}}
	rec = doGet(s, "/terminal/job/"+strings.ReplaceAll(`x<img src=x>`, " ", "%20"))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<img src=x>") {
		t.Fatal("agent name rendered unescaped")
	}
}

func TestJobTerminalStreamNoRunYet(t *testing.T) {
	reader := &fakeJobPodLogReader{}
	s, root := jobTerminalServer(t, reader)
	rec := doGet(s, "/terminal/job/deps/stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `no sandbox run yet for agent "deps"`) || !strings.Contains(body, root) {
		t.Fatalf("no-run state must be named and locate the workspace root:\n%s", body)
	}
	reader.mu.Lock()
	names := reader.launchNames
	reader.mu.Unlock()
	if len(names) != 1 || names[0] != sandbox.LaunchName("deps") {
		t.Fatalf("pods must be looked up by the executor's launch name, got %v", names)
	}
}

func writeTranscript(t *testing.T, root, agent string, unixNano int64, body string) string {
	t.Helper()
	p := filepath.Join(sandbox.WorkspaceDir(root, agent, unixNano), filepath.FromSlash(sandbox.TranscriptRelPath))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJobTerminalStreamShowsLastTranscriptWhenNoPod(t *testing.T) {
	reader := &fakeJobPodLogReader{}
	s, root := jobTerminalServer(t, reader)
	old := writeTranscript(t, root, "deps", 1, "stdout:\nold run\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"1\n")
	newest := writeTranscript(t, root, "deps", 2, "stdout:\nbumped golang.org/x/net\ntoken ghp_abcdefghijklmnopqrstuvwxyz0123456789\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"0\n")
	// Make the ordering unambiguous regardless of filesystem timestamp
	// resolution.
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(old, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newest, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	body := doGet(s, "/terminal/job/deps/stream").Body.String()
	for _, want := range []string{
		"no Job pod for agent \"deps\" right now",
		newest,
		"bumped golang.org/x/net",
		"last run exited with code 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "old run") || strings.Contains(body, "exited with code 1") {
		t.Fatalf("older transcript leaked into the pane:\n%s", body)
	}
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("token-shaped value reached the browser unscrubbed:\n%s", body)
	}

	// A legacy transcript without the exit line says so rather than
	// inventing a code.
	legacy := writeTranscript(t, root, "deps", 3, "stdout:\nlegacy\n\nstderr:\n")
	if err := os.Chtimes(legacy, base.Add(2*time.Minute), base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	body = doGet(s, "/terminal/job/deps/stream").Body.String()
	if !strings.Contains(body, "legacy") || !strings.Contains(body, "exit code was not recorded") {
		t.Fatalf("legacy transcript handling:\n%s", body)
	}
}

func TestJobTerminalStreamListErrorFallsBackWithRBACHint(t *testing.T) {
	reader := &fakeJobPodLogReader{listErr: errors.New(`GET /api/v1/namespaces/hive/pods: HTTP 403: pods is forbidden`)}
	s, root := jobTerminalServer(t, reader)
	writeTranscript(t, root, "deps", 1, "stdout:\nfrom disk\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"2\n")
	rec := doGet(s, "/terminal/job/deps/stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"cannot list Job pods", "pods is forbidden", "sandbox-job-rbac.yaml", "from disk", "last run exited with code 2"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestJobTerminalStreamFinishedPodDumpsLogAndExitCode(t *testing.T) {
	pod := kubejob.PodInfo{Name: "hive-kick-deps-1-abcde", Job: "hive-kick-deps-1", Phase: kubejob.PodPhaseFailed, Terminated: true, ExitCode: 3}
	reader := &fakeJobPodLogReader{pod: pod, found: true, logLines: []string{"compiling\n", "FAIL: tests\n"}}
	s, _ := jobTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/deps/stream").Body.String()
	for _, want := range []string{
		"job hive-kick-deps-1 · pod hive-kick-deps-1-abcde · Failed (exit code 3)",
		"compiling\nFAIL: tests\n",
		"kick container exited with code 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	reader.mu.Lock()
	followed := reader.followed
	reader.mu.Unlock()
	if len(followed) != 1 || followed[0] {
		t.Fatalf("a finished pod must not be followed, got %v", followed)
	}
}

// flushRecorder records each Flush so a test can wait for a line to have
// been pushed to the client rather than sleeping.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes chan string
}

func (f *flushRecorder) Flush() {
	f.ResponseRecorder.Flush()
	f.flushes <- f.Body.String()
}

func TestJobTerminalStreamFollowsRunningPodThenReportsExit(t *testing.T) {
	running := kubejob.PodInfo{Name: "p", Job: "j", Phase: kubejob.PodPhaseRunning}
	exited := kubejob.PodInfo{Name: "p", Job: "j", Phase: kubejob.PodPhaseSucceeded, Terminated: true, ExitCode: 0}
	gate := make(chan struct{})
	reader := &fakeJobPodLogReader{pod: running, found: true, logLines: []string{"step 1\n", "step 2\n"}, gate: gate, statuses: []kubejob.PodInfo{exited}}
	s, _ := jobTerminalServer(t, reader)

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushes: make(chan string, 64)}
	req := httptest.NewRequest(http.MethodGet, "/terminal/job/deps/stream", nil)
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()

	// The first line is flushed to the client while the stream is still
	// open: following, not buffering.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case body := <-rec.flushes:
			if strings.Contains(body, "step 1\n") {
				goto release
			}
		case <-deadline:
			t.Fatal("first log line never flushed while the pod was running")
		}
	}
release:
	close(gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not finish after the pod exited")
	}
	body := rec.Body.String()
	for _, want := range []string{"job j · pod p · Running", "step 1\nstep 2\n", "kick container exited with code 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	reader.mu.Lock()
	followed := reader.followed
	reader.mu.Unlock()
	if len(followed) != 1 || !followed[0] {
		t.Fatalf("a running pod must be followed, got %v", followed)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("stream must opt out of proxy buffering")
	}
}

func TestJobTerminalStreamWaitsForPendingPod(t *testing.T) {
	pending := kubejob.PodInfo{Name: "p", Job: "j", Phase: kubejob.PodPhasePending, Waiting: "ContainerCreating"}
	running := kubejob.PodInfo{Name: "p", Job: "j", Phase: kubejob.PodPhaseRunning}
	exited := kubejob.PodInfo{Name: "p", Job: "j", Phase: kubejob.PodPhaseSucceeded, Terminated: true, ExitCode: 0}
	reader := &fakeJobPodLogReader{pod: pending, found: true, logLines: []string{"hello\n"},
		statuses: []kubejob.PodInfo{pending, {Name: "p", Job: "j", Phase: kubejob.PodPhasePending, Waiting: "ImagePullBackOff"}, running, exited}}
	s, _ := jobTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/deps/stream").Body.String()
	for _, want := range []string{"Pending (ContainerCreating)", "Pending (ImagePullBackOff)", "pod p · Running", "hello\n", "kick container exited with code 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The pending poll must not have requested logs before the pod ran.
	reader.mu.Lock()
	followed := reader.followed
	reader.mu.Unlock()
	if len(followed) != 1 || !followed[0] {
		t.Fatalf("followed = %v", followed)
	}
}

func TestJobTerminalStreamClientGoneStopsPendingPoll(t *testing.T) {
	pending := kubejob.PodInfo{Name: "p", Phase: kubejob.PodPhasePending, Waiting: "ContainerCreating"}
	reader := &fakeJobPodLogReader{pod: pending, found: true, statuses: []kubejob.PodInfo{pending}}
	s, _ := jobTerminalServer(t, reader)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/terminal/job/deps/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler kept polling after the client went away")
	}
	reader.mu.Lock()
	followed := reader.followed
	reader.mu.Unlock()
	if len(followed) != 0 {
		t.Fatalf("no log request should be made for a pod that never started, got %v", followed)
	}
}

func TestJobTerminalStreamScrubsLiveLog(t *testing.T) {
	pod := kubejob.PodInfo{Name: "p", Phase: kubejob.PodPhaseSucceeded, Terminated: true}
	reader := &fakeJobPodLogReader{pod: pod, found: true, logLines: []string{"auth: ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok\n"}}
	s, _ := jobTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/deps/stream").Body.String()
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("live pod log reached the browser unscrubbed:\n%s", body)
	}
	if !strings.Contains(body, "auth: ") || !strings.Contains(body, " ok\n") {
		t.Fatalf("scrubbing must keep the surrounding line:\n%s", body)
	}
}

func TestJobTerminalStreamLogErrorIsReported(t *testing.T) {
	pod := kubejob.PodInfo{Name: "p", Phase: kubejob.PodPhaseFailed, Terminated: true, ExitCode: 137}
	reader := &fakeJobPodLogReader{pod: pod, found: true, logErr: errors.New("HTTP 403: pods/log is forbidden")}
	s, _ := jobTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/deps/stream").Body.String()
	if !strings.Contains(body, "log stream ended with an error: HTTP 403") || !strings.Contains(body, "exited with code 137") {
		t.Fatalf("body:\n%s", body)
	}
}

func TestJobTerminalDefaultsResolveInCluster(t *testing.T) {
	s, deps := apiServer(t)
	if _, ok := s.jobPodLogReader().(*kubejob.Launcher); !ok {
		t.Fatal("default reader must be the kubejob launcher's in-cluster client")
	}
	if s.jobTerminalPollInterval() != defaultJobTerminalPollInterval {
		t.Fatal("default poll interval")
	}
	t.Setenv("HIVE_WORK_DIR", "/work")
	if got := s.jobTerminalWorkspaceRoot(); got != sandbox.WorkspaceRoot("", "/work") {
		t.Fatalf("workspace root = %q", got)
	}
	deps.Config.AgentSandbox.WorkspaceDir = "/mnt/rwx/sandbox"
	if got := s.jobTerminalWorkspaceRoot(); got != "/mnt/rwx/sandbox" {
		t.Fatalf("configured workspace root must win, got %q", got)
	}
	s.deps = nil
	rec := doGet(s, "/terminal/job/deps/stream")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without config: status = %d", rec.Code)
	}
}

func TestAgentStatusExposesSandboxRuntime(t *testing.T) {
	on := true
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{AgentSandbox: config.AgentSandboxConfig{Enabled: true, Runtime: config.SandboxRuntimeJob}}}
	if got := effectiveSandboxRuntime(config.AgentConfig{}, cfg); got != "" {
		t.Fatalf("non-sandboxed agent must report no runtime, got %q", got)
	}
	if got := effectiveSandboxRuntime(config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Enabled: &on}}, cfg); got != config.SandboxRuntimeJob {
		t.Fatalf("global runtime not inherited, got %q", got)
	}
	if got := effectiveSandboxRuntime(config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: config.SandboxRuntimePodman}}, cfg); got != config.SandboxRuntimePodman {
		t.Fatalf("per-agent override not honoured, got %q", got)
	}
}

func TestStaticTerminalLinkRoutesJobRuntimeToPodLogViewer(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const JOB_SANDBOX_RUNTIME = 'job';",
		"function terminalUrl(agentName, sandboxRuntime)",
		"/terminal/job/${encodeURIComponent(agentName)}",
		`href="${terminalUrl(a.name, a.sandboxRuntime)}"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard job-terminal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "terminalUrl(a.name)}") {
		t.Fatal("a terminal link still ignores the agent's sandbox runtime")
	}
}
