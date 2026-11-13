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

// fakeSubstrateLogReader scripts sandbox.SubstrateLogReader: an optional
// recorded actor, the states it passes through, the output its bridge
// streams and the state it ends in.
type fakeSubstrateLogReader struct {
	mu          sync.Mutex
	info        sandbox.SubstrateActorInfo
	found       bool
	latestErr   error
	states      []sandbox.SubstrateActorInfo
	lines       []string
	final       sandbox.SubstrateActorInfo
	streamErr   error
	launchNames []string
	streamed    []string
	polls       []time.Duration
}

func (f *fakeSubstrateLogReader) LatestActor(_ context.Context, launchName string) (sandbox.SubstrateActorInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchNames = append(f.launchNames, launchName)
	return f.info, f.found, f.latestErr
}

func (f *fakeSubstrateLogReader) StreamRun(_ context.Context, name string, poll time.Duration, w io.Writer, onState func(sandbox.SubstrateActorInfo)) (sandbox.SubstrateActorInfo, error) {
	f.mu.Lock()
	f.streamed = append(f.streamed, name)
	f.polls = append(f.polls, poll)
	states, lines, final, err := f.states, f.lines, f.final, f.streamErr
	f.mu.Unlock()
	for _, st := range states {
		onState(st)
	}
	for _, l := range lines {
		if _, werr := io.WriteString(w, l); werr != nil {
			return final, werr
		}
	}
	return final, err
}

// substrateTerminalServer adds a substrate-runtime agent "actor" to the
// job-terminal fixture (which already has job "deps", podman "lint" and the
// tmux agent "scanner").
func substrateTerminalServer(t *testing.T, reader *fakeSubstrateLogReader) (*Server, string) {
	t.Helper()
	s, root := jobTerminalServer(t, &fakeJobPodLogReader{})
	on := true
	s.deps.Config.Agents["actor"] = config.AgentConfig{Backend: "claude", Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: config.SandboxRuntimeSubstrate}}
	s.jobTerminal.substrate = reader
	return s, root
}

const testSpiffeID = "spiffe://ate.local/ns/ate-workers/sa/worker"

func TestJobTerminalGateAdmitsSubstrateRuntime(t *testing.T) {
	if !sandboxRuntimeWithoutTmux(config.SandboxRuntimeSubstrate) {
		t.Fatal("substrate agents have no tmux session; the viewer must admit them")
	}
	s, _ := substrateTerminalServer(t, &fakeSubstrateLogReader{})
	rec := doGet(s, "/terminal/job/actor")
	if rec.Code != http.StatusOK {
		t.Fatalf("substrate agent page: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"actor — substrate actor log", "Substrate actor", "'./actor/stream'"} {
		if !strings.Contains(body, want) {
			t.Errorf("substrate page missing %q", want)
		}
	}
	if strings.Contains(body, "Kubernetes Job") || strings.Contains(body, "OpenShell") {
		t.Fatal("substrate page must not describe the kick as a Job or OpenShell sandbox")
	}
	if rec := doGet(s, "/terminal/job/lint/stream"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Substrate actor") {
		t.Fatalf("podman agent stream: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSubstrateTerminalStreamsKickWithActorTransitionsScrubbed(t *testing.T) {
	token := "ghp_" + strings.Repeat("Z", 30)
	reader := &fakeSubstrateLogReader{
		info:  sandbox.SubstrateActorInfo{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateResuming},
		found: true,
		states: []sandbox.SubstrateActorInfo{
			{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateRunning, SpiffeID: testSpiffeID},
			// Only the exit code changed: no new status line.
			{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateRunning, SpiffeID: testSpiffeID, Exited: true, ExitCode: 4},
			{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateSuspended, SpiffeID: testSpiffeID, Exited: true, ExitCode: 4, Suspended: true, Done: true},
		},
		lines: []string{"step 1\n", "auth " + token + " ok\n", "partial"},
		final: sandbox.SubstrateActorInfo{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateSuspended, SpiffeID: testSpiffeID, Exited: true, ExitCode: 4, Suspended: true, Done: true},
	}
	s, _ := substrateTerminalServer(t, reader)
	rec := doGet(s, "/terminal/job/actor/stream")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"substrate actor hive/hive-kick-actor-1 · RESUMING",
		"this is the kick's stdout and stderr as the actor's bridge streams /run",
		"actor hive/hive-kick-actor-1 · RUNNING · " + testSpiffeID,
		"actor hive/hive-kick-actor-1 · SUSPENDED · " + testSpiffeID,
		"step 1\n",
		"auth ghp_",
		"partial",
		// Unlike OpenShell, the bridge's exit code is the kick's own.
		"kick exited with code 4",
		"actor hive-kick-actor-1 was suspended with a snapshot (snapshot_on_idle): the kick has finished",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "· RUNNING ·"); n != 1 {
		t.Errorf("RUNNING reported %d times; a state is reported once per transition:\n%s", n, body)
	}
	if strings.Contains(body, token) {
		t.Fatalf("live kick output reached the browser unscrubbed:\n%s", body)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.launchNames) != 1 || reader.launchNames[0] != sandbox.LaunchName("actor") {
		t.Fatalf("actor must be looked up by the executor's launch name, got %v", reader.launchNames)
	}
	if len(reader.streamed) != 1 || reader.streamed[0] != "hive-kick-actor-1" || reader.polls[0] != time.Millisecond {
		t.Fatalf("streamed %v at %v", reader.streamed, reader.polls)
	}
}

func TestSubstrateTerminalStreamReportsErrorAndDeletedActor(t *testing.T) {
	reader := &fakeSubstrateLogReader{
		info: sandbox.SubstrateActorInfo{Name: "a", State: sandbox.SubstrateStateRunning}, found: true,
		final:     sandbox.SubstrateActorInfo{Name: "a", State: sandbox.SubstrateStateDeleting, Done: true},
		streamErr: errors.New("router refused connect"),
	}
	s, _ := substrateTerminalServer(t, reader)
	body := doGet(s, "/terminal/job/actor/stream").Body.String()
	for _, want := range []string{"output stream ended with an error: router refused connect", "actor a was deleted by the launcher: the kick has finished. the full output and exit code are in the persisted transcript"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "kick exited with code") {
		t.Fatalf("no exit code was reported; none must be shown:\n%s", body)
	}

	reader = &fakeSubstrateLogReader{
		info: sandbox.SubstrateActorInfo{Name: "b", State: sandbox.SubstrateStateRunning}, found: true,
		final: sandbox.SubstrateActorInfo{Name: "b", State: sandbox.SubstrateStateRunning},
	}
	s, _ = substrateTerminalServer(t, reader)
	if body := doGet(s, "/terminal/job/actor/stream").Body.String(); !strings.Contains(body, "actor b · RUNNING (exit code not reported)") {
		t.Fatalf("unfinished stream:\n%s", body)
	}
}

// Between kicks the recorded actor is finished: the pane names it and how
// it ended (suspended with snapshot_on_idle), then shows the transcript.
func TestSubstrateTerminalFinishedActorShowsTranscript(t *testing.T) {
	reader := &fakeSubstrateLogReader{
		info:  sandbox.SubstrateActorInfo{Name: "hive-kick-actor-1", Atespace: "hive", State: sandbox.SubstrateStateSuspended, SpiffeID: testSpiffeID, Exited: true, Suspended: true, Done: true},
		found: true,
	}
	s, root := substrateTerminalServer(t, reader)
	path := writeTranscript(t, root, "actor", 1, "stdout:\nfrom the last kick\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"0\n")
	body := doGet(s, "/terminal/job/actor/stream").Body.String()
	for _, want := range []string{
		"substrate actor hive/hive-kick-actor-1 · SUSPENDED · " + testSpiffeID,
		"actor hive-kick-actor-1 was suspended with a snapshot (snapshot_on_idle)",
		path, "from the last kick", "last run exited with code 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	reader.mu.Lock()
	streamed := reader.streamed
	reader.mu.Unlock()
	if len(streamed) != 0 {
		t.Fatalf("finished actor: nothing must be streamed, got %v", streamed)
	}
}

func TestSubstrateTerminalFallsBackToTranscript(t *testing.T) {
	reader := &fakeSubstrateLogReader{}
	s, root := substrateTerminalServer(t, reader)

	body := doGet(s, "/terminal/job/actor/stream").Body.String()
	if !strings.Contains(body, `no sandbox run yet for agent "actor": no Substrate actor started by this hive process`) || !strings.Contains(body, root) {
		t.Fatalf("no-run state:\n%s", body)
	}

	path := writeTranscript(t, root, "actor", 1, "stdout:\nfrom the last kick\n\nstderr:\n\n\n"+sandbox.TranscriptExitPrefix+"0\n")
	body = doGet(s, "/terminal/job/actor/stream").Body.String()
	for _, want := range []string{`no Substrate actor for agent "actor" right now`, path, "from the last kick", "last run exited with code 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	reader.latestErr = errors.New("registry unavailable")
	body = doGet(s, "/terminal/job/actor/stream").Body.String()
	for _, want := range []string{"cannot read Substrate actors", "registry unavailable", "from the last kick"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestSubstrateTerminalDefaultReaderIsLauncherRecord(t *testing.T) {
	s, _ := apiServer(t)
	if r, ok := s.substrateLogReader().(*sandbox.SubstrateKicks); !ok || r != sandbox.DefaultSubstrateKicks {
		t.Fatal("default reader must be the record the substrate launcher writes")
	}
}

func TestStaticTerminalLinkRoutesSubstrateRuntimeToViewer(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const SUBSTRATE_SANDBOX_RUNTIME = 'substrate';",
		"|| sandboxRuntime === SUBSTRATE_SANDBOX_RUNTIME;",
		"if (a.sandboxRuntime === SUBSTRATE_SANDBOX_RUNTIME) {",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard substrate-terminal wiring missing %q", want)
		}
	}
}
