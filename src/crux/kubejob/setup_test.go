package kubejob

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// hivecommons/hive#19574: sandbox.setup runs after the preflight and before the
// agent; a setup failure is setup_failed (no retry), a health failure is
// node_unhealthy (retry elsewhere).

const (
	setupFailLog  = "pip: mirror unreachable\n" + setupMarker + " setup command 1 of 1 exited 1: ./install.sh\n"
	healthFailLog = "torch: no device\n" + setupHealthMarker + " health command 1 of 1 exited 1: python -c smoke\n"
)

func setupOptions() Options {
	return Options{Setup: []string{"./install.sh"}, SetupHealth: []string{"python -c smoke"}, RelayImage: RelayImageNone}
}

// runSetupScript executes the real setup wrapper with /bin/sh in dir, as the
// Job's container would in its workspace, and returns its output and exit code.
func runSetupScript(t *testing.T, o Options, dir string, agent []string, env ...string) (string, int) {
	t.Helper()
	argv := setupCommand(agent, o)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the setup script: %v", err)
	}
	return string(out), code
}

func TestSetupScriptRunsSetupThenHealthThenAgentInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	o := Options{
		Setup:       []string{"echo step-one", "touch installed.flag"},
		SetupHealth: []string{"test -f installed.flag && echo health-ok"},
	}
	out, code := runSetupScript(t, o, dir, []string{"echo", "agent-started"})
	if code != 0 {
		t.Fatalf("code=%d out=%q, want setup and health to pass", code, out)
	}
	order := []string{"step-one", "health-ok", "agent-started"}
	last := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i < 0 || i < last {
			t.Fatalf("output %q: want %v in that order", out, order)
		}
		last = i
	}
	if _, err := os.Stat(filepath.Join(dir, "installed.flag")); err != nil {
		t.Fatalf("setup did not run in the workspace: %v", err)
	}
	if strings.Contains(out, setupMarker) || strings.Contains(out, setupHealthMarker) {
		t.Fatalf("a passing setup printed a failure marker: %q", out)
	}
}

func TestSetupScriptFailedInstallIsSetupFailed(t *testing.T) {
	o := Options{Setup: []string{"true", "echo mirror-down >&2; exit 4"}, SetupHealth: []string{"echo health-ran"}}
	out, code := runSetupScript(t, o, t.TempDir(), []string{"echo", "agent-started"})
	if code != ExitCodeSetupFailed {
		t.Fatalf("exit code = %d, want %d: %q", code, ExitCodeSetupFailed, out)
	}
	if strings.Contains(out, "agent-started") || strings.Contains(out, "health-ran") {
		t.Fatalf("the health checks or the agent ran after a failed setup: %q", out)
	}
	for _, want := range []string{"mirror-down", setupMarker + " setup command 2 of 2 exited 4: echo mirror-down"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestSetupScriptFailedHealthIsNodeUnhealthy(t *testing.T) {
	o := Options{Setup: []string{"true"}, SetupHealth: []string{"echo no-device >&2; exit 2"}}
	out, code := runSetupScript(t, o, t.TempDir(), []string{"echo", "agent-started"})
	if code != ExitCodeNodeUnhealthy {
		t.Fatalf("exit code = %d, want %d: %q", code, ExitCodeNodeUnhealthy, out)
	}
	if strings.Contains(out, "agent-started") {
		t.Fatalf("the agent ran after a failed health check: %q", out)
	}
	if !strings.Contains(out, setupHealthMarker+" health command 1 of 1 exited 2") {
		t.Errorf("output %q lacks the health failure line", out)
	}
}

func TestSetupScriptHealthOnlyAndTimeout(t *testing.T) {
	out, code := runSetupScript(t, Options{SetupHealth: []string{"echo only-health"}}, t.TempDir(), []string{"echo", "agent-started"})
	if code != 0 || !strings.Contains(out, "only-health") || !strings.Contains(out, "agent-started") {
		t.Fatalf("health-only setup: code=%d out=%q", code, out)
	}
	start := time.Now()
	out, code = runSetupScript(t, Options{Setup: []string{"exec sleep 30"}, SetupTimeout: time.Second}, t.TempDir(), []string{"echo", "agent-started"})
	if code != ExitCodeSetupFailed || !strings.Contains(out, "timed out after 1s") {
		t.Fatalf("code=%d out=%q, want a setup timeout", code, out)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("the hung setup command was not cut off: took %s", time.Since(start))
	}
}

func TestSetupScriptLogsNoCredential(t *testing.T) {
	const secret = "ghp_setupsecretvalue01234567890123"
	out, _ := runSetupScript(t, Options{Setup: []string{"exit 1"}}, t.TempDir(), []string{"true"}, "GITHUB_TOKEN="+secret)
	if strings.Contains(out, secret) {
		t.Fatalf("the setup output carries a credential from the environment: %q", out)
	}
}

func TestSetupCommandUnchangedWithoutConfig(t *testing.T) {
	cmd := []string{"claude", "-p"}
	if got := setupCommand(cmd, Options{}); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("no setup must leave the command alone, got %q", got)
	}
	if got := setupCommand(cmd, Options{Setup: []string{" "}, SetupHealth: []string{""}}); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("blank setup entries must leave the command alone, got %q", got)
	}
	if got := setupCommand(nil, Options{Setup: []string{"true"}}); got != nil {
		t.Fatalf("an empty command cannot be wrapped, got %q", got)
	}
	argv := setupCommand([]string{"agent"}, Options{Setup: []string{"a"}, SetupHealth: []string{"b", "c"}})
	if argv[3] != setupScriptName || argv[4] != "900" || argv[5] != "1800" || argv[6] != "1" || argv[7] != "2" {
		t.Fatalf("default per/total/count args = %v", argv[3:8])
	}
	if got := argv[8:]; !reflect.DeepEqual(got, []string{"a", "b", "c", "agent"}) {
		t.Fatalf("commands then agent, got %q", got)
	}
}

func TestManifestOrdersRelayPreflightSetupAgent(t *testing.T) {
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data", Preflight: []string{"nvidia-smi"}, Setup: []string{"./install.sh"}, SetupHealth: []string{"python -c smoke"}}}
	cmd := firstContainerCommand(t, l.jobManifest("hive", "hive-kick-x", "deps", agentSpec(), &relayPlan{image: "hive:test"}))
	at := func(name string) int {
		for i, c := range cmd {
			if c == name {
				return i
			}
		}
		t.Fatalf("command lacks %q: %q", name, cmd)
		return -1
	}
	relay, pre, setup, agent := at("hive-relay-preflight"), at("hive-node-preflight"), at(setupScriptName), at("@.hive/kick-prompt.txt")
	if relay >= pre || pre >= setup || setup >= agent {
		t.Fatalf("want relay check, preflight, setup, agent in that order: %q", cmd)
	}
	if at("./install.sh") >= at("python -c smoke") {
		t.Fatalf("setup commands must come before health commands: %q", cmd)
	}
}

func TestManifestWithoutSetupIsUnchanged(t *testing.T) {
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data"}}
	m := manifestJSON(t, l.jobManifest("hive", "hive-kick-x", "deps", agentSpec(), nil))
	if strings.Contains(m, setupScriptName) {
		t.Fatalf("setup script in a manifest that configures none: %s", m)
	}
}

func TestRunSetupFailureIsSetupFailedWithoutRetry(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCodes = []int{ExitCodeSetupFailed}
	api.podNodes = []string{"node-a", "node-b"}
	api.logs = []string{setupFailLog}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, setupOptions())

	res, err := l.Run(context.Background(), agentSpec())
	var fe *sandbox.FailError
	if !errors.As(err, &fe) || fe.Reason != sandbox.FailReasonSetupFailed || fe.Node != "node-a" {
		t.Fatalf("Run error = %v, want setup_failed on node-a", err)
	}
	if !strings.Contains(fe.Message, setupMarker) || !strings.Contains(fe.Message, "./install.sh") {
		t.Errorf("message = %q, want the failing command", fe.Message)
	}
	if api.jobCreations != 1 {
		t.Errorf("Jobs created = %d, want no retry on another node", api.jobCreations)
	}
	if res.FailReason != sandbox.FailReasonSetupFailed || !strings.Contains(res.Stdout, "mirror unreachable") {
		t.Errorf("result = %+v", res)
	}
}

func TestRunSetupHealthFailureRetriesOnAnotherNode(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed, statusComplete}
	api.exitCodes = []int{ExitCodeNodeUnhealthy, 0}
	api.podNodes = []string{"node-a", "node-b"}
	api.logs = []string{healthFailLog, "agent ok\n"}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	opts := setupOptions()
	var ev events
	opts.OnEvent = ev.add
	l := newLauncher(srv, opts)

	res, err := l.Run(context.Background(), agentSpec())
	if err != nil {
		t.Fatalf("Run: %v (the retry on node-b passed)", err)
	}
	if api.jobCreations != 2 || !strings.Contains(manifestJSON(t, api.createdAll[1]), `"values":["node-a"]`) {
		t.Fatalf("creations=%d, want one retry that avoids node-a", api.jobCreations)
	}
	if res.Node != "node-b" || res.FailReason != "" || !strings.Contains(res.Stdout, setupHealthMarker) {
		t.Errorf("result = %+v", res)
	}
	if got := ev.kinds(); !reflect.DeepEqual(got, []string{EventNodeRetry}) {
		t.Errorf("events = %v, want one node_retry", got)
	}
}

func TestRunAgentExitingWithTheSetupCodeIsAnOrdinaryFailure(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCode = ExitCodeSetupFailed
	api.podNodes = []string{"node-a"}
	api.logBody = "agent exited 70 on its own\n"
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, setupOptions())

	res, err := l.Run(context.Background(), agentSpec())
	var fe *sandbox.FailError
	if err == nil || errors.As(err, &fe) || res.FailReason != "" {
		t.Fatalf("err=%v result=%+v: an exit 70 without the marker is an ordinary failure", err, res)
	}
}

func TestRunOnlyAgentKicksGetTheSetup(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, setupOptions())
	spec := specFor(preflightWorkspace) // Relays unset: a publish.verify launch
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cmd := firstContainerCommand(t, api.created); !reflect.DeepEqual(cmd, spec.Command) {
		t.Fatalf("a non-agent launch ran the setup step: %q", cmd)
	}
	if len(l.Options.Setup) != 1 || len(l.Options.SetupHealth) != 1 {
		t.Error("the shared launcher's options were modified")
	}
}

func TestSetupOptionsValidation(t *testing.T) {
	good := Options{WorkspaceClaim: "c", Setup: []string{"true"}, SetupHealth: []string{"true"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid setup refused: %v", err)
	}
	for name, mutate := range map[string]func(*Options){
		"empty setup":      func(o *Options) { o.Setup = []string{"true", " "} },
		"empty health":     func(o *Options) { o.SetupHealth = []string{""} },
		"negative timeout": func(o *Options) { o.SetupTimeout = -time.Second },
		"negative total":   func(o *Options) { o.SetupTotalTimeout = -time.Second },
		"too many":         func(o *Options) { o.Setup = make([]string, config.MaxSandboxSetupCommands+1) },
	} {
		o := good
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}
