package agent

import (
	"strings"
	"testing"
)

// backendLaunchCmd is the single source of truth for how each backend's CLI is
// launched inside a tmux pane. The agy and codex branches already have
// dedicated contract tests (agy_launch_test.go, codex_launch_test.go); these
// tests cover the remaining branches, with particular weight on the claude and
// copilot branches because they carry the GitHub-MCP write-deny security
// contract: agents must author via the App-gated gh wrapper, never as the
// login user via the MCP.

func TestBackendLaunchCmd_ClaudeCarriesWriteAndHostStateDenies(t *testing.T) {
	got := backendLaunchCmd("claude", "claude-fable-5", "claude", false, "")

	if !strings.HasPrefix(got, "claude --model claude-fable-5 --dangerously-skip-permissions") {
		t.Fatalf("claude launch cmd missing base contract: %q", got)
	}
	if strings.Contains(got, "--bare") {
		t.Errorf("non-inference claude launch must not carry --bare: %q", got)
	}
	if !strings.Contains(got, claudeGitHubWriteDenyFlags) {
		t.Errorf("claude launch cmd missing GitHub MCP write-deny flags: %q", got)
	}
	if want := claudeHostStateDenyFlags(); want != "" && !strings.Contains(got, want) {
		t.Errorf("claude launch cmd missing host-state deny flags %q: %q", want, got)
	}
}

func TestBackendLaunchCmd_ClaudeInferenceAddsBareSettings(t *testing.T) {
	got := backendLaunchCmd("claude", "m", "claude", true, "")

	if !strings.Contains(got, " --bare --settings "+claudeInferenceSettingsPath) {
		t.Fatalf("inference claude launch must carry --bare --settings %s: %q",
			claudeInferenceSettingsPath, got)
	}
	// The write-deny contract applies in every mode, inference included.
	if !strings.Contains(got, claudeGitHubWriteDenyFlags) {
		t.Errorf("inference claude launch cmd missing GitHub MCP write-deny flags: %q", got)
	}
}

func TestBackendLaunchCmd_CopilotDeniesMCPWritesAndPassesModelThrough(t *testing.T) {
	got := backendLaunchCmd("copilot", "auto", "copilot", false, "")

	if !strings.HasPrefix(got, "copilot --model auto --no-auto-update --allow-all") {
		t.Fatalf("copilot launch cmd missing base contract: %q", got)
	}
	if !strings.Contains(got, copilotGitHubWriteDenyFlags) {
		t.Errorf("copilot launch cmd missing GitHub MCP write-deny flags: %q", got)
	}
	// PRIMARY defense: the flag that would register the MCP write tools must
	// never appear.
	if strings.Contains(got, "--enable-all-github-mcp-tools") {
		t.Errorf("copilot launch cmd must never enable all GitHub MCP tools: %q", got)
	}
}

func TestBackendLaunchCmd_EveryMCPWriteToolDeniedForClaudeAndCopilot(t *testing.T) {
	// The two deny lists must stay the same logical set. Guard the set here so
	// a tool added to one spelling but not the other fails loudly.
	writeTools := []string{
		"create_pull_request",
		"create_pull_request_with_copilot",
		"merge_pull_request",
		"create_issue",
		"update_issue",
		"add_issue_comment",
	}
	claudeCmd := backendLaunchCmd("claude", "m", "claude", false, "")
	copilotCmd := backendLaunchCmd("copilot", "m", "copilot", false, "")
	for _, tool := range writeTools {
		if want := "--disallowed-tools 'mcp__github__" + tool + "'"; !strings.Contains(claudeCmd, want) {
			t.Errorf("claude launch cmd missing deny for %s: %q", tool, claudeCmd)
		}
		if want := "--deny-tool='github-mcp-server(" + tool + ")'"; !strings.Contains(copilotCmd, want) {
			t.Errorf("copilot launch cmd missing deny for %s: %q", tool, copilotCmd)
		}
	}
}

func TestBackendLaunchCmd_SimpleModelFlagBackends(t *testing.T) {
	for _, backend := range []string{"gemini", "pi"} {
		got := backendLaunchCmd("bin", "some-model", backend, false, "")
		if want := "bin --model some-model"; got != want {
			t.Errorf("backendLaunchCmd(%s) = %q, want %q", backend, got, want)
		}
	}
}

func TestBackendLaunchCmd_GooseRunWithAndWithoutModel(t *testing.T) {
	if got, want := backendLaunchCmd("goose", "", "goose", false, ""), "goose run -s"; got != want {
		t.Errorf("goose without model = %q, want %q", got, want)
	}
	if got, want := backendLaunchCmd("goose", "gpt-6-astra", "goose", false, ""),
		"goose run -s --model gpt-6-astra"; got != want {
		t.Errorf("goose with model = %q, want %q", got, want)
	}
}

func TestBackendLaunchCmd_UnknownBackendFallsBackToBareBinary(t *testing.T) {
	if got, want := backendLaunchCmd("somebin", "ignored-model", "mystery", false, "high"), "somebin"; got != want {
		t.Errorf("unknown backend = %q, want bare binary %q", got, want)
	}
}

func TestBackendLaunchCmd_BobDelegatesToBobLaunchCmd(t *testing.T) {
	if got, want := backendLaunchCmd("bobbin", "m", bobBackend, false, ""), bobLaunchCmd("bobbin"); got != want {
		t.Errorf("bob backend = %q, want bobLaunchCmd contract %q", got, want)
	}
}

func TestCodexEffortFlag(t *testing.T) {
	if got := codexEffortFlag(""); got != "" {
		t.Errorf("codexEffortFlag(\"\") = %q, want empty", got)
	}
	if got, want := codexEffortFlag("xhigh"), ` -c model_reasoning_effort="xhigh"`; got != want {
		t.Errorf("codexEffortFlag(xhigh) = %q, want %q", got, want)
	}
}

func TestSanitizeRestartReason(t *testing.T) {
	if got, want := sanitizeRestartReason("   "), "operator"; got != want {
		t.Errorf("blank reason = %q, want %q", got, want)
	}
	if got, want := sanitizeRestartReason(" tmux pane died "), "tmux pane died"; got != want {
		t.Errorf("trimmed reason = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 200)
	if got := sanitizeRestartReason(long); len(got) != 80 {
		t.Errorf("long reason truncated to %d chars, want 80", len(got))
	}
}

func TestPaneShowsEmptyInputPrompt(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"bare prompt last line", "some output\n" + cliInputPromptMarker + "\n", true},
		{"prompt with trailing blank lines", cliInputPromptMarker + "\n\n  \n", true},
		{"prompt with typed text", cliInputPromptMarker + " half-typed command\n", false},
		{"no prompt at all", "compiling...\ndone\n", false},
		{"empty pane", "", false},
		{"prompt above later output", cliInputPromptMarker + "\nnew output after prompt\n", false},
	}
	for _, tc := range cases {
		if got := paneShowsEmptyInputPrompt(tc.pane); got != tc.want {
			t.Errorf("%s: paneShowsEmptyInputPrompt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackendLaunchCmd_OMPPassesModelEffortAndApproval(t *testing.T) {
	t.Setenv("HIVE_OMP_APPROVAL_MODE", "")
	got := backendLaunchCmd("omp", "openai-codex/gpt-5.6-terra", "omp", false, "xhigh")
	want := "omp --approval-mode yolo --model openai-codex/gpt-5.6-terra --thinking xhigh"
	if got != want {
		t.Fatalf("omp launch = %q, want %q", got, want)
	}
}

func TestBackendLaunchCmd_OMPApprovalModeIsConfigurable(t *testing.T) {
	t.Setenv("HIVE_OMP_APPROVAL_MODE", "auto-approve")
	got := backendLaunchCmd("omp", "anthropic/claude-opus-5", "omp", false, "")
	want := "omp --approval-mode auto-approve --model anthropic/claude-opus-5"
	if got != want {
		t.Fatalf("omp launch with custom approval = %q, want %q", got, want)
	}
}
