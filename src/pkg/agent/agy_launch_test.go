package agent

// Launch-command test for the agy (Antigravity CLI) backend added in #3910.
// The contract under pin is the flag set the commit's analysis established:
//
//   - --dangerously-skip-permissions is ALWAYS passed — without it agy blocks
//     on a per-tool approval prompt no one is attached to answer;
//   - a configured model is passed as --model <m> --effort <effort>, deriving
//     the effort from agy's -low/-medium/-high model suffix when the agent has
//     no explicit reasoning_effort, because agy silently IGNORES --model
//     without --effort — dropping or mismatching the effort flag would make the
//     configured model a no-op while looking fine.
//
// This test used to launch a real tmux pane with an agy stub and poll
// CaptureFullLog for ~35s waiting for the typed command to echo back. That is
// environment- and timing-dependent: in CI the pane frequently never
// materialized, the capture came back EMPTY, and the whole package failed —
// which zeroed pkg/agent's coverage score and re-fired the coverage floor
// issue even though no package had actually regressed (#5299).
//
// The flag contract lives in backendLaunchCmd, a pure function, so it is
// asserted directly here. The assertions themselves are unchanged: same flags,
// same model, same reason strings.

import (
	"strings"
	"testing"
)

// agyLaunchCmd builds the agy launch command the way Start does for an agent
// with no ToolsConfig: normalize the configured model for the backend, then
// hand it to backendLaunchCmd. Keeping the normalization step means the test
// still covers the model plumbing, not just the fmt.Sprintf.
func agyInteractiveLaunchCmdForTest(t *testing.T, model, effort string) string {
	t.Helper()
	t.Setenv(agyLaunchModeEnv, agyLaunchModeInteractive)
	const backend = "agy"
	isInference := IsInferenceBackend(backend)
	return backendLaunchCmd("agy", normalizeModelNameForBackend(model, backend, isInference), backend, isInference, effort)
}

func TestAgyLaunchCommandLine_DefaultsToHeadlessShim(t *testing.T) {
	t.Setenv(agyLaunchModeEnv, "")
	cmd := agyLaunchCmdForTest(t, "gemini-pro", "")

	if strings.Contains(cmd, "agy --dangerously-skip-permissions") {
		t.Fatalf("default agy launch must not start the CPU-hungry interactive TUI; cmd: %q", cmd)
	}
	if !strings.Contains(cmd, agyHeadlessReadyMarker) {
		t.Fatalf("default agy launch must install the headless ready marker; cmd: %q", cmd)
	}
}

func agyLaunchCmdForTest(t *testing.T, model, effort string) string {
	t.Helper()
	const backend = "agy"
	isInference := IsInferenceBackend(backend)
	return backendLaunchCmd("agy", normalizeModelNameForBackend(model, backend, isInference), backend, isInference, effort)
}

// TestStart_AgyLaunchCommandLine asserts the command line agy is launched with
// carries the #3910 flag contract.
func TestStart_AgyLaunchCommandLine(t *testing.T) {
	// "gemini-pro" survives normalizeModelName unchanged (no trailing digit
	// segment), so the assertion below sees the configured model verbatim.
	cmd := agyInteractiveLaunchCmdForTest(t, "gemini-pro", "")

	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("agy launched without --dangerously-skip-permissions — it will block on a per-tool approval prompt no one answers; cmd: %q", cmd)
	}
	if !strings.Contains(cmd, "--model gemini-pro --effort "+agyDefaultEffort) {
		t.Errorf("agy launched without '--model gemini-pro --effort %s' — agy silently ignores --model without --effort, so the configured model would never take effect; cmd: %q",
			agyDefaultEffort, cmd)
	}
}

// TestAgyLaunchCommandLine_NoModel pins the other half of the contract: with no
// model configured, agy still gets the bypass flag, and it must NOT be given a
// bare --model/--effort pair built from an empty model.
func TestAgyLaunchCommandLine_NoModel(t *testing.T) {
	cmd := agyInteractiveLaunchCmdForTest(t, "", "")

	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("agy must get --dangerously-skip-permissions even with no model configured; cmd: %q", cmd)
	}
	if strings.Contains(cmd, "--model") || strings.Contains(cmd, "--effort") {
		t.Errorf("agy with no configured model must not be passed --model/--effort; cmd: %q", cmd)
	}
}

// TestAgyLaunchCommandLine_ConfiguredEffort pins the per-agent
// reasoning_effort plumbing: an effort agy accepts replaces agyDefaultEffort,
// and one agy rejects (codex's wider vocabulary) falls back to the default
// rather than making agy ignore the model outright.
func TestAgyLaunchCommandLine_ConfiguredEffort(t *testing.T) {
	if cmd := agyInteractiveLaunchCmdForTest(t, "gemini-pro", "high"); !strings.Contains(cmd, "--model gemini-pro --effort high") {
		t.Errorf("configured effort 'high' must reach agy's --effort; cmd: %q", cmd)
	}
	if cmd := agyInteractiveLaunchCmdForTest(t, "gemini-pro", "xhigh"); !strings.Contains(cmd, "--model gemini-pro --effort "+agyDefaultEffort) {
		t.Errorf("effort agy rejects must fall back to --effort %s when the model has no effort suffix; cmd: %q", agyDefaultEffort, cmd)
	}
}

func TestAgyLaunchCommandLine_DerivesEffortFromModelSuffix(t *testing.T) {
	for _, c := range []struct {
		model string
		want  string
	}{
		{"gemini-3.8-flash-high", "high"},
		{"gemini-3.8-flash-medium", "medium"},
		{"gemini-3.8-flash-low", "low"},
		{"gemini-pro-high", "high"},
	} {
		cmd := agyInteractiveLaunchCmdForTest(t, c.model, "")
		want := "--model " + c.model + " --effort " + c.want
		if !strings.Contains(cmd, want) {
			t.Errorf("agy model suffix must set effort; missing %q in cmd: %q", want, cmd)
		}
	}

	cmd := agyInteractiveLaunchCmdForTest(t, "gemini-3.8-flash-high", "medium")
	if !strings.Contains(cmd, "--model gemini-3.8-flash-high --effort medium") {
		t.Errorf("explicit effort must override model suffix; cmd: %q", cmd)
	}

	cmd = agyInteractiveLaunchCmdForTest(t, "gemini-3.8-flash-high", "xhigh")
	if !strings.Contains(cmd, "--model gemini-3.8-flash-high --effort high") {
		t.Errorf("invalid effort must fall back to model suffix rather than downgrading; cmd: %q", cmd)
	}
}

func TestAgyHeadlessTurnCommandRunsTurnRunner(t *testing.T) {
	cmd := agyHeadlessTurnShellCommand("/usr/local/bin/hive", "agy", "gemini-pro", "high", "/data/agents/a/.hive-agy-prompt-a.txt", false)

	if strings.Contains(cmd, "do the task") || strings.Contains(cmd, "$(cat") {
		t.Fatalf("headless turn command must not expand the prompt into argv (E2BIG past 128 KiB); cmd: %q", cmd)
	}
	for _, want := range []string{
		"'/usr/local/bin/hive' agy-turn --agy 'agy' --model 'gemini-pro' --effort 'high' --prompt-file '/data/agents/a/.hive-agy-prompt-a.txt'",
		`--conversation-file "${` + agyConversationFileVar + `:-}"`,
		agyHeadlessRunningMarker,
		agyHeadlessReadyMarker,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("headless turn command missing %q; cmd: %q", want, cmd)
		}
	}
	if strings.Contains(cmd, "--new-conversation") {
		t.Errorf("a normal kick must continue the agent's conversation; cmd: %q", cmd)
	}
	if cmd := agyHeadlessTurnShellCommand("hive", "agy", "", "", "/p", true); !strings.Contains(cmd, "--new-conversation") {
		t.Errorf("ClearOnKick must start a new conversation; cmd: %q", cmd)
	}
	if cmd := agyHeadlessTurnShellCommand("hive", "agy", "", "high", "/p", false); strings.Contains(cmd, "--model") || strings.Contains(cmd, "--effort") {
		t.Errorf("no model configured must pass neither --model nor --effort; cmd: %q", cmd)
	}
}

func TestAgyHeadlessMarkersDrivePaneReadiness(t *testing.T) {
	if !paneHasCLIMarker("prefix " + agyHeadlessReadyMarker + " suffix") {
		t.Fatal("agy headless ready marker must count as a live backend")
	}
	if !paneShowsInputPrompt(agyHeadlessReadyMarker + "> ") {
		t.Fatal("agy headless ready marker must count as an input prompt")
	}
	if !paneShowsAgentWorking(agyHeadlessRunningMarker) {
		t.Fatal("agy headless running marker must keep kick delivery from interrupting an active turn")
	}
	if paneShowsAgentWorking(agyHeadlessRunningMarker + "\n" + agyHeadlessReadyMarker + "> ") {
		t.Fatal("agy headless ready marker must clear an older running marker in the visible pane")
	}
}
