package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestClinePodmanExecutorStagingAndFailureCleanup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	source := filepath.Join(root, "dedicated-provider")
	settings := filepath.Join(source, "data", "settings")
	if err := os.MkdirAll(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	const key = "dummy-private-provider-key"
	if err := os.WriteFile(filepath.Join(settings, "providers.json"),
		[]byte(`{"version":1,"providers":{"openrouter":{"settings":{"apiKey":"`+key+`"},"tokenSource":"manual"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_CLINE_CONFIG_DIR", source)
	agent := &AgentProcess{Config: config.AgentConfig{Model: "openrouter/test"}}
	if err := validateClineSandbox(agent, "podman", config.SandboxJobConfig{}); err != nil {
		t.Fatal(err)
	}
	launcher := &clineFileCaptureLauncher{key: key}
	e := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
	res, err := e.Run(context.Background(), SandboxKickSpec{Agent: "cline-worker", Runtime: "podman",
		AgentConfig: configSnapshot{Backend: "cline", Model: "openrouter/test"}, Message: "bounded task",
		Org: "example", Repo: "repo", WorkspaceDir: filepath.Join(root, "workspaces"), Image: "cline-image"})
	if err == nil {
		t.Fatal("fixture failure was lost")
	}
	if launcher.file == "" {
		t.Fatal("launcher did not receive scoped provider file")
	}
	if _, err := os.Stat(launcher.file); !os.IsNotExist(err) {
		t.Fatal("executor retained provider stage after launcher error")
	}
	transcript, err := os.ReadFile(res.TranscriptPath)
	if err != nil || strings.Contains(string(transcript), key) || strings.Contains(res.Sandbox.Stderr, key) {
		t.Fatal("credential reached transcript/result", err)
	}
	launcher = &clineFileCaptureLauncher{key: key, waitForTimeout: true}
	e = &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
	const fixtureKickTimeout = 10 * time.Millisecond
	res, err = e.Run(context.Background(), SandboxKickSpec{Agent: "cline-worker", Runtime: "podman",
		AgentConfig: configSnapshot{Backend: "cline", Model: "openrouter/test"}, Message: "bounded task",
		Org: "example", Repo: "repo", WorkspaceDir: filepath.Join(root, "timeout-workspaces"), Image: "cline-image",
		Timeout: fixtureKickTimeout})
	if !res.TimedOut || err == nil || !strings.Contains(err.Error(), "hive-cline-provider-fixture") || !strings.Contains(err.Error(), "timed out") {
		t.Fatal("timeout discarded actionable container cleanup diagnostics", err)
	}
	if _, err := os.Stat(launcher.file); !os.IsNotExist(err) {
		t.Fatal("provider stage survived timeout")
	}
}

type clineFileCaptureLauncher struct {
	file, key      string
	waitForTimeout bool
}

func (l *clineFileCaptureLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	if spec.ClineProvider == nil {
		return sandbox.Result{}, errors.New("missing capability")
	}
	var err error
	l.file, err = spec.ClineProvider.MountFile(spec.Workspace)
	if err != nil {
		return sandbox.Result{}, err
	}
	if l.waitForTimeout {
		<-ctx.Done()
		return sandbox.Result{Stderr: l.key, ExitCode: -1}, errors.New("Cline container cleanup failed; remove hive-cline-provider-fixture before retrying")
	}
	return sandbox.Result{Stderr: l.key, ExitCode: 1}, errors.New("fixture failure")
}
