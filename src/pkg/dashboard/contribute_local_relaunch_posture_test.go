package dashboard

import (
	"strings"
	"testing"
)

// kubestellar/hive#5652: the relay's post-task relaunch dropped local mode's
// sandbox. `just contribute-hive <backend> local` resolves a NARROWED launch
// line (claude: --permission-mode dontAsk + native sandbox settings + workspace
// write-allowlist; copilot: --sandbox; opencode: the host-state deny-list) —
// but that resolution lived only in the Justfile, at first launch. The relay
// re-derived its relaunch line from backends.conf's backend_perm_flag, which
// answers with the CONTAINER posture, so the first task exit relaunched a
// sandboxed local agent as `claude --dangerously-skip-permissions
// --permission-mode bypassPermissions` for the rest of the session.
//
// The fix is single-sourcing: the Justfile resolves the launch line once,
// types it into the pane, and exports the SAME value to the relay as
// AGENT_LAUNCH_CMD; buildLaunchCommand() in bin/contributor-relay.js prefers
// that over its own derivation. The launcher half of that contract is pinned by
// TestContributeHiveExportsLaunchCommandForRelayRelaunch in
// contribute_pane_cwd_test.go; this file pins the CONSUMER half and the
// cross-file agreement. The relay's behaviour itself — a relaunch byte-
// identical to the exported line, sandbox kept when the launch had one, none
// invented when the operator opted out, container fallback intact — is pinned
// behaviorally in bin/contributor-relay.test.js ("#5652 ...").

// localModeLaunchBlock returns the local-mode text from the branch head through
// the relay start, covering both the pane launch and the relay's environment.
func localModeLaunchBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, `if [[ "$_MODE" == "local" ]]; then`)
	if start < 0 {
		t.Fatal("contribute-hive local-mode branch not found in the Justfile")
	}
	end := strings.Index(src[start:], "cleanup() {")
	if end < 0 {
		t.Fatal("end of the local-mode relay start block not found in the Justfile")
	}
	return src[start : start+end]
}

// TestRelayReadsTheExactVariableTheLauncherExports pins the env-var contract
// across the two files. A rename on either side would fail silently at
// runtime — the variable would simply be absent in the relay's environment,
// and the permissive backends.conf fallback would win again, which is exactly
// the #5652 escape. Neither file's own tests can catch that: each side would
// still be self-consistent.
func TestRelayReadsTheExactVariableTheLauncherExports(t *testing.T) {
	const launchCmdVar = "AGENT_LAUNCH_CMD"

	block := localModeLaunchBlock(t)
	if !strings.Contains(block, "export "+launchCmdVar+"=") {
		t.Fatalf("the Justfile local branch no longer exports %s; "+
			"relaunches would re-derive the CONTAINER posture and drop the sandbox (#5652)", launchCmdVar)
	}

	relay := fileSource(t, "bin/contributor-relay.js")
	if !strings.Contains(relay, "process.env."+launchCmdVar) {
		t.Fatalf("bin/contributor-relay.js no longer reads %s; "+
			"local-mode relaunches would fall back to the container posture (#5652)", launchCmdVar)
	}
}

// TestRelayBuildLaunchCommandPrefersTheEntrypointLine pins that the exported
// line is consulted in buildLaunchCommand() itself — the single source every
// relaunch path (task exit, crash restart, stall backstop, revoke) goes
// through — not merely read somewhere in the file.
func TestRelayBuildLaunchCommandPrefersTheEntrypointLine(t *testing.T) {
	relay := fileSource(t, "bin/contributor-relay.js")
	fnStart := strings.Index(relay, "function buildLaunchCommand()")
	if fnStart < 0 {
		t.Fatal("buildLaunchCommand() not found in bin/contributor-relay.js")
	}
	buildFn := relay[fnStart:]
	if end := strings.Index(buildFn, "\n}"); end > 0 {
		buildFn = buildFn[:end]
	}
	if !strings.Contains(buildFn, "ENTRYPOINT_LAUNCH_CMD") {
		t.Error("buildLaunchCommand() no longer prefers the entrypoint-resolved launch line (#5652)")
	}
}

// TestLocalModePaneLaunchNeverBypassesTheExportedLine pins the vulnerable
// shape out of existence: a send-keys line interpolating $CMD $PERM_FLAG
// directly again would relaunch from a DIFFERENT construction than the one
// exported to the relay — two derivations of one launch line, which is how
// #5652 (and #2203 bug 1 before it) happened.
func TestLocalModePaneLaunchNeverBypassesTheExportedLine(t *testing.T) {
	block := localModeLaunchBlock(t)
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "send-keys") && strings.Contains(line, "$PERM_FLAG") {
			t.Errorf("a local-mode pane launch interpolates $PERM_FLAG directly, bypassing AGENT_LAUNCH_CMD: %s",
				strings.TrimSpace(line))
		}
	}
}
