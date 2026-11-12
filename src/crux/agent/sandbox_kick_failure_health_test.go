package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
	"github.com/hivecommons/hive/crux/sandbox"
)

type kickHealthLauncher struct {
	entered    chan struct{}
	allow      chan struct{}
	started    chan struct{}
	finish     chan struct{}
	failBefore bool
}

func (l *kickHealthLauncher) Run(ctx context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	close(l.entered)
	select {
	case <-l.allow:
	case <-ctx.Done():
		return sandbox.Result{}, ctx.Err()
	}
	if l.failBefore {
		return sandbox.Result{}, errors.New("launcher rejected run")
	}
	if spec.OnStarted != nil {
		spec.OnStarted()
	}
	close(l.started)
	select {
	case <-l.finish:
		return sandbox.Result{}, errors.New("execution failed after acceptance")
	case <-ctx.Done():
		return sandbox.Result{}, ctx.Err()
	}
}

func sandboxKickHealthManager(t *testing.T, launcher sandbox.Launcher) *Manager {
	t.Helper()
	yes := true
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}, quietTestLogger(), ProjectContext{Org: "kubestellar", Repos: []string{"hive"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(launcher)
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSandboxKickHealthCountsLauncherFailure(t *testing.T) {
	launcher := &kickHealthLauncher{entered: make(chan struct{}), allow: make(chan struct{}), failBefore: true}
	m := sandboxKickHealthManager(t, launcher)
	if started, err := m.SendKickAsync("scanner", "task"); !started || err != nil {
		t.Fatalf("queue = %v, %v", started, err)
	}
	select {
	case <-launcher.entered:
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatal("launcher not reached")
	}
	status, err := m.GetStatus("scanner")
	if err != nil || status.ConsecutiveKickFailures != 0 || status.LastKickError != "" {
		t.Fatalf("queued status = %+v, %v", status, err)
	}
	close(launcher.allow)
	status = testutil.EventuallyValue(t, sandboxStateTransitionTimeout, func() (*crux.Process, bool) {
		status, _ := m.GetStatus("scanner")
		return status, status != nil && status.State == StateFailed
	}, "sandbox launch did not fail")
	if status.ConsecutiveKickFailures != 1 || !strings.Contains(status.LastKickError, "launcher rejected run") {
		t.Fatalf("failed status = %+v", status)
	}
}

func TestSandboxKickHealthResetsOnAcceptanceNotQueue(t *testing.T) {
	launcher := &kickHealthLauncher{entered: make(chan struct{}), allow: make(chan struct{}), started: make(chan struct{}), finish: make(chan struct{})}
	m := sandboxKickHealthManager(t, launcher)
	m.mu.Lock()
	recordKickFailureLocked(m.agents["scanner"], errors.New("previous failure"))
	m.mu.Unlock()
	if err := m.SendKick("scanner", "task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-launcher.entered:
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatal("launcher not reached")
	}
	status, err := m.GetStatus("scanner")
	if err != nil || status.ConsecutiveKickFailures != 1 || status.LastKickError != "previous failure" {
		t.Fatalf("queue reset failure streak: %+v, %v", status, err)
	}
	close(launcher.allow)
	select {
	case <-launcher.started:
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatal("launcher did not accept")
	}
	status, err = m.GetStatus("scanner")
	if err != nil || status.ConsecutiveKickFailures != 0 || status.LastKickError != "" {
		t.Fatalf("acceptance did not reset streak: %+v, %v", status, err)
	}
	close(launcher.finish)
	status = testutil.EventuallyValue(t, sandboxStateTransitionTimeout, func() (*crux.Process, bool) {
		status, _ := m.GetStatus("scanner")
		return status, status != nil && status.State == StateFailed
	}, "sandbox run did not finish")
	if status.ConsecutiveKickFailures != 0 || status.LastKickError != "" {
		t.Fatalf("post-acceptance execution error counted as delivery failure: %+v", status)
	}
}
