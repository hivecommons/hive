package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// backendsConfPath is the shell half of this policy. Both launch paths must
// deny the same commands; a test that only checked the Go side would let them
// drift, and the drift is invisible until an agent on the other path reaches
// something this one blocks.
const backendsConfPath = "../../../config/backends.conf"

// TestHostStateDenyBlocksTheReportedCommand pins the specific failure #4918
// reported: `rpm-ostree kargs` against the operator's real deployment.
func TestHostStateDenyBlocksTheReportedCommand(t *testing.T) {
	flags := claudeHostStateDenyFlags()
	if !strings.Contains(flags, "Bash(rpm-ostree:*)") {
		t.Fatalf("host-state denials do not cover rpm-ostree, the command in #4918: %q", flags)
	}
	if !strings.HasPrefix(flags, " --disallowed-tools ") {
		t.Fatalf("flags = %q, want a --disallowed-tools fragment", flags)
	}
}

// TestHostStateDenyCoversEscalationAndBootState pins the two families the list
// is built from. rpm-ostree asked polkit directly rather than shelling out to
// sudo, so escalation alone would not have stopped #4918 — both halves have to
// be present for the list to mean what its comment says.
func TestHostStateDenyCoversEscalationAndBootState(t *testing.T) {
	for _, want := range []string{
		"Bash(sudo:*)", "Bash(pkexec:*)", "Bash(doas:*)", "Bash(su:*)",
		"Bash(rpm-ostree:*)", "Bash(bootc:*)", "Bash(ostree:*)",
		"Bash(grubby:*)", "Bash(bootctl:*)", "Bash(efibootmgr:*)",
	} {
		if !strings.Contains(claudeHostStateDenyTools, want) {
			t.Errorf("host-state deny list is missing %s", want)
		}
	}
}

// TestHostStateDenyIsOneArgvWord guards the constraint that makes this work at
// all. The shell path word-splits its flag string, so a space anywhere in the
// pattern list would arrive as separate argv words and silently deny nothing —
// the failure mode where the flag is present and the policy is absent.
func TestHostStateDenyIsOneArgvWord(t *testing.T) {
	if strings.ContainsAny(claudeHostStateDenyTools, " \t\n") {
		t.Fatalf("deny list contains whitespace and would be word-split: %q", claudeHostStateDenyTools)
	}
}

// TestHostStateDenyOptOut covers the escape hatch, including that it accepts
// exactly the spellings the shell's is_truthy accepts.
func TestHostStateDenyOptOut(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "yes", "YES", "on", "ON"} {
		t.Setenv(hostStateBypassEnv, on)
		if got := claudeHostStateDenyFlags(); got != "" {
			t.Errorf("%s=%q still emitted denials: %q", hostStateBypassEnv, on, got)
		}
	}
	// Anything else keeps the denials. "false" and "0" matter most: an operator
	// who writes them means "leave the protection on", and reading them as
	// "set, therefore truthy" would invert the flag.
	for _, off := range []string{"", "0", "false", "no", "off", "maybe"} {
		t.Setenv(hostStateBypassEnv, off)
		if got := claudeHostStateDenyFlags(); got == "" {
			t.Errorf("%s=%q dropped the denials; only truthy values may", hostStateBypassEnv, off)
		}
	}
}

// TestGitHubWriteDenialsSurvive: the host-state denials are appended alongside
// the GitHub MCP write denials, not in place of them.
func TestGitHubWriteDenialsSurvive(t *testing.T) {
	combined := claudeGitHubWriteDenyFlags + claudeHostStateDenyFlags()
	if !strings.Contains(combined, "mcp__github__create_pull_request") {
		t.Error("GitHub MCP write denials were lost")
	}
	if !strings.Contains(combined, "Bash(rpm-ostree:*)") {
		t.Error("host-state denials were lost")
	}
}

// TestShellAndGoDenyListsAgree is the parity check. config/backends.conf is the
// relay path and this package is the pod path; an agent must be confined the
// same way whichever launched it.
func TestShellAndGoDenyListsAgree(t *testing.T) {
	if _, err := os.Stat(backendsConfPath); err != nil {
		testutil.SkipfUnlessRequired(t, "backends.conf not reachable from the test working directory: %v", err)
	}
	out, err := exec.Command("bash", "-c",
		"source "+backendsConfPath+" && printf '%s' \"$CLAUDE_HOST_DENY_TOOLS\"").Output()
	if err != nil {
		t.Fatalf("sourcing backends.conf: %v", err)
	}
	if got := string(out); got != claudeHostStateDenyTools {
		t.Fatalf("shell and Go deny lists have drifted:\n shell: %s\n    go: %s", got, claudeHostStateDenyTools)
	}
}
