package agent

// Launch-command test for the agy (Antigravity CLI) backend added in #3910.
// The contract under pin is the flag set the commit's analysis established:
//
//   - --dangerously-skip-permissions is ALWAYS passed — without it agy blocks
//     on a per-tool approval prompt no one is attached to answer;
//   - a configured model is passed as --model <m> --effort <agyDefaultEffort>,
//     because agy silently IGNORES --model without --effort — dropping the
//     effort flag would make the configured model a no-op while looking fine.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// installAgyStub writes an agy stub into the stub bin dir already on PATH
// (TestMain does not pre-create one — agy postdates the original stub list).
// It renders the ❯ ready marker so readiness gates resolve and no launch
// goroutine outlives the test, then echoes stdin like the other stubs.
func installAgyStub(t *testing.T) {
	t.Helper()
	p := filepath.Join(stubBinDir, "agy")
	script := "#!/bin/sh\nprintf '\\342\\235\\257 ready\\n'\nexec cat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("writing agy stub: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
}

// TestStart_AgyLaunchCommandLine launches a real agy-backed agent (stub
// binary, real tmux) and asserts the command line actually typed into the
// pane carries the #3910 flag contract. The pane is read via CaptureFullLog
// so wrapped lines are joined before matching.
func TestStart_AgyLaunchCommandLine(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	forceFastPaneShell(t)
	installAgyStub(t)

	// "gemini-pro" survives normalizeModelName unchanged (no trailing digit
	// segment), so the assertion below sees the configured model verbatim.
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("agy", "gemini-pro"),
	}, discardLogger(), ProjectContext{})

	if err := m.Start(t.Context(), "worker"); err != nil {
		t.Fatalf("Start(agy): %v", err)
	}
	defer cleanupAgent(t, m, "worker")

	// The launch line is typed into the pane in chunks; poll the joined
	// capture until the full command is visible.
	deadline := time.Now().Add(30 * time.Second)
	var pane string
	for time.Now().Before(deadline) {
		out, err := m.CaptureFullLog("worker")
		if err == nil && strings.Contains(out, "--effort") {
			pane = out
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if pane == "" {
		out, _ := m.CaptureFullLog("worker")
		t.Fatalf("agy launch command never appeared in the pane; capture: %q", out)
	}

	if !strings.Contains(pane, "--dangerously-skip-permissions") {
		t.Error("agy launched without --dangerously-skip-permissions — it will block on a per-tool approval prompt no one answers")
	}
	if !strings.Contains(pane, "--model gemini-pro --effort "+agyDefaultEffort) {
		t.Errorf("agy launched without '--model gemini-pro --effort %s' — agy silently ignores --model without --effort, so the configured model would never take effect; pane: %q",
			agyDefaultEffort, pane)
	}
}
