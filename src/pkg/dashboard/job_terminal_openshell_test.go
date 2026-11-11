package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/pkg/config"
)

// fakeOpenShellLogReader scripts sandbox.OpenShellLogReader: an optional
// live sandbox, the lines its log yields and the state it ends in.
type fakeOpenShellLogReader struct {
	mu          sync.Mutex
	info        sandbox.OpenShellSandboxInfo
	found       bool
	listErr     error
	lines       []string
	final       sandbox.OpenShellSandboxInfo
	streamErr   error
	launchNames []string
	streamed    []string
	polls       []time.Duration
}

func (f *fakeOpenShellLogReader) LatestSandbox(_ context.Context, launchName string) (sandbox.OpenShellSandboxInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchNames = append(f.launchNames, launchName)
	return f.info, f.found, f.listErr
}

func (f *fakeOpenShellLogReader) StreamLogs(_ context.Context, name string, poll time.Duration, w io.Writer) (sandbox.OpenShellSandboxInfo, error) {
	f.mu.Lock()
	f.streamed = append(f.streamed, name)
	f.polls = append(f.polls, poll)
	lines, final, err := f.lines, f.final, f.streamErr
	f.mu.Unlock()
	for _, l := range lines {
		if _, werr := io.WriteString(w, l); werr != nil {
			return final, werr
		}
	}
	return final, err
}

// openshellTerminalServer adds an openshell-runtime agent "shell" to the
// job-terminal fixture (which already has job "deps", podman "lint" and the
// tmux agent "scanner").
func openshellTerminalServer(t *testing.T, reader *fakeOpenShellLogReader) (*Server, string) {
	t.Helper()
	s, root := jobTerminalServer(t, &fakeJobPodLogReader{})
	on := true
	s.deps.Config.Agents["shell"] = config.AgentConfig{Backend: "claude", Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: config.SandboxRuntimeOpenShell}}
	s.jobTerminal.openshell = reader
	return s, root
}

func TestJobTerminalGateAdmitsRuntimesWithoutTmux(t *testing.T) {
	for rt, want := range map[string]bool{
		config.SandboxRuntimeJob:       true,
		config.SandboxRuntimeOpenShell: true,
		config.SandboxRuntimePodman:    false,
		"":                             false,
	} {
		if got := sandboxRuntimeWithoutTmux(rt); got != want {
			t.Errorf("sandboxRuntimeWithoutTmux(%q) = %v, want %v", rt, got, want)
		}
	}
	s, _ := openshellTerminalServer(t, &fakeOpenShellLogReader{})
	rec := doGet(s, "/terminal/job/shell")
	if rec.Code != http.StatusOK {
		t.Fatalf("openshell agent page: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"shell — openshell sandbox log", "OpenShell sandbox", "'./shell/stream'"} {
		if !strings.Contains(body, want) {
			t.Errorf("openshell page missing %q", want)
		}
	}
	if strings.Contains(body, "Kubernetes Job") {
		t.Fatal("openshell page must not describe the kick as a Kubernetes Job")
	}
	// The job agent's page is unchanged and podman still has no viewer.
	if body := doGet(s, "/terminal/job/deps").Body.String(); !strings.Contains(body, "deps — sandbox job log") || !strings.Contains(body, "Kubernetes Job in its own pod") {
		t.Fatalf("job page changed:\n%s", body)
	}
	if rec := doGet(s, "/terminal/job/lint/stream"); rec.Code != http.StatusConflict {
		t.Fatalf("podman agent stream: status = %d", rec.Code)
	}
}

func TestOpenShellTerminalStreamsLiveSandboxScrubbed(t *testing.T) {
	token := "ghp_" + strings.Repeat("Z", 30)
	reader := &fakeOpenShellLogReader{
		info:  sandbox.OpenShellSandboxInfo{Name: "hive-kick-shell-1", Phase: "Ready"},
		found: true,
		lines: []string{"step 1\n", "auth " + token + " ok\n", "partial"},
		final: sandbox.OpenShellSandboxInfo{Name: "hive-kick-shell-1", Phase: "Completed", Exited: true, ExitCode: 4},
	}
	s, _ := openshellTerminalServer(t, reader)
	rec := doGet(s, "/terminal/job/shell/stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"openshell sandbox hive-kick-shell-1 · Ready",
		// The gateway log is not the kick's output; the pane must say so up front.
		"this is the sandbox's gateway log (supervisor, policy and tracing events), not the kick's output",
		"step 1\n",
		"auth ghp_",
		"partial",
		// Status.ExitCode belongs to the sandbox's main process, never the kick.
		"sandbox hive-kick-shell-1 main process exited with code 4 (that is the sandbox workload, not the kick)",
		"the kick's output and exit code are in the persisted transcript",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "kick exited with code") {
		t.Fatalf("the sandbox exit code must not be presented as the kick's:\n%s", body)
	}
	if strings.Contains(body, token) {
		t.Fatalf("live sandbox log reached the browser unscrubbed:\n%s", body)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.launchNames) != 1 || reader.launchNames[0] != sandbox.LaunchName("shell") {
		t.Fatalf("sandbox must be looked up by the executor's launch name, got %v", reader.launchNames)
	}
	if len(reader.streamed) != 1 || reader.streamed[0] != "hive-kick-shell-1" || reader.polls[0] != time.Millisecond {
		t.Fatalf("streamed %v at %v", reader.streamed, reader.polls)
	}
}

func TestOpenShellTerminalStreamReportsErrorAndMissingExit(t *testing.T) {
	reader := &fakeOpenShellLogReader{
		info: sandbox.OpenShellSandboxInfo{Name: "sb", Phase: "Ready"}, found: true,
		final:     sandbox.OpenShellSandboxInfo{Name: "sb", Phase: "Error"},
		streamErr: errors.New("PermissionDenied: logs"),
	}
	s, _ := openshellTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/shell/stream").Body.String()
	for _, want := range []string{"log stream ended with an error: PermissionDenied: logs", "sandbox sb · Error. the kick's output and exit code are in the persisted transcript"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// The launcher deletes the sandbox as soon as the kick's exec returns, so a
// stream that ends on NotFound is the normal end of a kick: the pane must
// say the kick finished and send the operator to the transcript rather than
// print a missing or wrong exit code.
func TestOpenShellTerminalStreamEndsWhenLauncherDeletesSandbox(t *testing.T) {
	reader := &fakeOpenShellLogReader{
		info: sandbox.OpenShellSandboxInfo{Name: "hive-kick-shell-1", Phase: "Ready"}, found: true,
		lines: []string{"NET:OPEN ALLOWED api.github.com:443\n"},
		final: sandbox.OpenShellSandboxInfo{Name: "hive-kick-shell-1", Phase: sandbox.OpenShellPhaseDeleting},
	}
	s, _ := openshellTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/shell/stream").Body.String()
	for _, want := range []string{
		"NET:OPEN ALLOWED api.github.com:443\n",
		"sandbox hive-kick-shell-1 was deleted by the launcher: the kick has finished. the kick's output and exit code are in the persisted transcript; reopen this pane to see them",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	for _, bad := range []string{"exited with code", "exit code not reported"} {
		if strings.Contains(body, bad) {
			t.Fatalf("unexpected %q in:\n%s", bad, body)
		}
	}
}

func TestOpenShellTerminalFallsBackToTranscript(t *testing.T) {
	reader := &fakeOpenShellLogReader{}
	s, root := openshellTerminalServer(t, reader)

	body := doGet(s, "/terminal/job/shell/stream").Body.String()
	if !strings.Contains(body, `no sandbox run yet for agent "shell": no OpenShell sandbox on the gateway`) || !strings.Contains(body, root) {
		t.Fatalf("no-run state:\n%s", body)
	}

	path := writeTranscript(t, root, "shell", 1, "stdout:\nfrom the last kick\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"0\n")
	body = doGet(s, "/terminal/job/shell/stream").Body.String()
	for _, want := range []string{`no OpenShell sandbox for agent "shell" right now`, path, "from the last kick", "last run exited with code 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	reader.mu.Lock()
	streamed := reader.streamed
	reader.mu.Unlock()
	if len(streamed) != 0 {
		t.Fatalf("no live sandbox: nothing must be streamed, got %v", streamed)
	}

	reader.listErr = errors.New("Unavailable: gateway down")
	body = doGet(s, "/terminal/job/shell/stream").Body.String()
	for _, want := range []string{"cannot list OpenShell sandboxes", "gateway down", "gateway_url", "from the last kick"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestOpenShellTerminalDefaultReaderUsesAgentGateway(t *testing.T) {
	s, deps := apiServer(t)
	on := true
	deps.Config.AgentSandbox = config.AgentSandboxConfig{Enabled: true, Image: "img"}
	deps.Config.Agents["shell"] = config.AgentConfig{Enabled: true, Sandbox: &config.AgentSandboxOverride{
		Enabled: &on, Runtime: config.SandboxRuntimeOpenShell,
		OpenShell: &config.SandboxOpenShellConfig{GatewayURL: "https://gw.example:8443", Namespace: "team", TokenEnv: "HIVE_TEST_OPENSHELL_TOKEN"},
	}}
	t.Setenv("HIVE_TEST_OPENSHELL_TOKEN", "tok")
	l, ok := s.openshellLogReader("shell").(*sandbox.OpenShellLauncher)
	if !ok {
		t.Fatal("default reader must be the sandbox package's OpenShell launcher")
	}
	if l.Options.GatewayURL != "https://gw.example:8443" || l.Options.Workspace != "team" || l.Options.Token != os.Getenv("HIVE_TEST_OPENSHELL_TOKEN") {
		t.Fatalf("options = %+v", l.Options)
	}
}

func TestStaticTerminalLinkRoutesOpenShellRuntimeToViewer(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const OPENSHELL_SANDBOX_RUNTIME = 'openshell';",
		"function sandboxRuntimeWithoutTmux(sandboxRuntime)",
		"if (sandboxRuntimeWithoutTmux(sandboxRuntime)) {",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard openshell-terminal wiring missing %q", want)
		}
	}
}
