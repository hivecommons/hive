package agent

// Launch-command test for the agy (Antigravity CLI) backend added in #3910.
// The contract under pin is the flag set the commit's analysis established:
//
//   - --dangerously-skip-permissions is ALWAYS passed — without it agy blocks
//     on a per-tool approval prompt no one is attached to answer;
//   - a configured model is passed as --model <m> --effort <agyDefaultEffort>,
//     because agy silently IGNORES --model without --effort — dropping the
//     effort flag would make the configured model a no-op while looking fine.
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
func agyLaunchCmd(t *testing.T, model string) string {
	t.Helper()
	const backend = "agy"
	isInference := IsInferenceBackend(backend)
	return backendLaunchCmd("agy", normalizeModelNameForBackend(model, backend, isInference), backend, isInference)
}

// TestStart_AgyLaunchCommandLine asserts the command line agy is launched with
// carries the #3910 flag contract.
func TestStart_AgyLaunchCommandLine(t *testing.T) {
	// "gemini-pro" survives normalizeModelName unchanged (no trailing digit
	// segment), so the assertion below sees the configured model verbatim.
	cmd := agyLaunchCmd(t, "gemini-pro")

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
	cmd := agyLaunchCmd(t, "")

	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("agy must get --dangerously-skip-permissions even with no model configured; cmd: %q", cmd)
	}
	if strings.Contains(cmd, "--model") || strings.Contains(cmd, "--effort") {
		t.Errorf("agy with no configured model must not be passed --model/--effort; cmd: %q", cmd)
	}
}
