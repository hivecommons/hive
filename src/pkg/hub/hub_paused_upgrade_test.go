package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeKubectl(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// A paused hub deployment: no set image, no watcher, fault surfaced once.
func TestRolloutHubToSHAPausedDeployment(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	writeFakeKubectl(t, "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncase \"$*\" in *spec.paused*) echo true;; esac\nexit 0\n")

	old := hubImageExists
	hubImageExists = func(string, *slog.Logger) bool { return true }
	t.Cleanup(func() { hubImageExists = old })

	s := bulkTestHub(t)
	for i := 0; i < 2; i++ {
		if err := s.rolloutHubToSHA("abc1234"); err == nil {
			t.Fatal("expected an error while the deployment is paused")
		}
	}
	calls, _ := os.ReadFile(logPath)
	if strings.Contains(string(calls), "set image") || strings.Contains(string(calls), "rollout status") {
		t.Fatalf("paused deployment must not be patched or watched, calls:\n%s", calls)
	}
	if got := s.HubUpgradeFault(); got != hubPausedFaultMessage {
		t.Fatalf("fault = %q, want %q", got, hubPausedFaultMessage)
	}
	if s.hubWatcherRunning {
		t.Fatal("no watcher may start for a paused deployment")
	}
}

// The readiness probe must not use the blocking --timeout=0s form and must
// return within a bounded time when the rollout is incomplete.
func TestHubRolloutReadyNonBlocking(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	writeFakeKubectl(t, "#!/bin/sh\necho \"$*\" >> "+logPath+"\nexit 1\n")
	s := bulkTestHub(t)
	start := time.Now()
	if s.hubRolloutReady() {
		t.Fatal("incomplete rollout reported ready")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("readiness probe did not return promptly")
	}
	calls, _ := os.ReadFile(logPath)
	if !strings.Contains(string(calls), "--watch=false") || strings.Contains(string(calls), "--timeout=0s") {
		t.Fatalf("probe must use --watch=false, got: %s", calls)
	}
}

// Repeated triggers start at most one watcher.
func TestRolloutHubToSHASingleWatcher(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	// rollout status sleeps so the first watcher stays alive across triggers.
	writeFakeKubectl(t, "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncase \"$*\" in *rollout*) sleep 1; exit 1;; esac\nexit 0\n")
	old := hubImageExists
	hubImageExists = func(string, *slog.Logger) bool { return true }
	t.Cleanup(func() { hubImageExists = old })

	s := bulkTestHub(t)
	if err := s.rolloutHubToSHA("abc1234"); err != nil {
		t.Fatal(err)
	}
	if err := s.rolloutHubToSHA("abc1234"); err != nil {
		t.Fatal(err)
	}
	s.hubUpgradeMu.Lock()
	running := s.hubWatcherRunning
	s.hubUpgradeMu.Unlock()
	if !running {
		t.Fatal("first trigger should have started a watcher")
	}
	// The watcher's first probe happens after hubRolloutPollInterval (15s), so
	// no rollout-status call can have happened yet; the flag is the guard.
}
