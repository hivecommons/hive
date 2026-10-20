package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func writeFakeTmux(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	waitExecutable(t, path)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// waitExecutable runs the freshly written script once, retrying while a
// concurrent fork still holds its write descriptor (ETXTBSY, golang/go#22315).
// The fake scripts are side-effect free, so the probe run is harmless.
func waitExecutable(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		err := exec.Command(path, "-V").Run()
		if !errors.Is(err, syscall.ETXTBSY) {
			return
		}
		<-time.After(5 * time.Millisecond)
	}
	t.Fatalf("%s still reports text file busy", path)
}

func TestTmuxSessionExistsLogsProbeFailureStderr(t *testing.T) {
	writeFakeTmux(t, `#!/bin/sh
echo "probe stderr says permission denied" >&2
exit 1
`)
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, nil))
	m := NewManager(map[string]config.AgentConfig{"guide": {Backend: "claude"}}, logger, ProjectContext{})
	m.mu.RLock()
	agent := m.agents["guide"]
	m.mu.RUnlock()

	if m.tmuxSessionExistsForAgent(agent) {
		t.Fatal("tmuxSessionExistsForAgent = true, want false")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "tmux has-session failed") || !strings.Contains(logs, "probe stderr says permission denied") {
		t.Fatalf("probe failure log missing stderr: %s", logs)
	}
}

func TestCrashRestartDuplicateSessionDoesNotIncrementRestartCount(t *testing.T) {
	writeFakeTmux(t, `#!/bin/sh
case "$*" in
  *has-session*) echo "transient probe failure" >&2; exit 1 ;;
  *kill-session*) echo "kill failed but server stayed alive" >&2; exit 1 ;;
  *new-session*) echo "duplicate session: hive-guide" >&2; exit 1 ;;
esac
exit 0
`)
	dir := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", dir)
	m := NewManager(map[string]config.AgentConfig{"guide": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["guide"]
	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	agent.tmuxSession = "hive-guide"
	m.mu.Unlock()

	if restarted := m.CheckAndRestartCrashedAgents(context.Background()); len(restarted) != 0 {
		t.Fatalf("CheckAndRestartCrashedAgents restarted %v, want duplicate-session adoption only", restarted)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent = m.agents["guide"]
	if agent.RestartCount != 0 || len(agent.RestartEvents) != 0 {
		t.Fatalf("restart telemetry changed: count=%d events=%d", agent.RestartCount, len(agent.RestartEvents))
	}
	if agent.State != StateRunning {
		t.Fatalf("state = %s, want running", agent.State)
	}
	if agent.cancel == nil {
		t.Fatal("existing session monitor was not restored")
	}
	agent.cancel()
}
