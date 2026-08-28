package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #4918: `just contribute-hive <backend> local` runs the backend CLI as the
// contributor's own user, on their own machine. Claude/litellm, codex, and
// copilot now use a native OS-enforced sandbox on that path; opencode gets a
// command-name deny-list (a floor, not a boundary) via its own permission
// config; goose, agy, bob, pi, and aider have no confinement mechanism this
// repo can wire at all and REFUSE to launch locally without an explicit
// per-backend opt-in env var. See contribute_local_mode_backend_matrix_test.go
// for the tests covering the five refuse-to-launch backends, copilot's
// sandbox gating, and opencode's deny-list.
//
// What the silence cost: an agent doing entirely correct work on an assigned
// third-party repo ran that repo's own test suite; a latent defect in two of
// its tests let a hook escape its stubs and call `rpm-ostree kargs` against the
// operator's REAL deployment, raising three polkit dialogs on their desktop.
// Nothing was written, and the only reason is that the process happened to lack
// privilege. No compromise was involved.
//
// Container mode remains the backend-independent remedy and the default.

// contributeHiveLocalBranch returns the head of contribute-hive's local-mode
// branch, up to the tmux session name it derives. Read from the Justfile rather
// than restated, so the warning cannot be dropped quietly.
func contributeHiveLocalBranch(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, `if [[ "$_MODE" == "local" ]]; then`)
	if start < 0 {
		t.Fatal("contribute-hive local-mode branch not found in the Justfile")
	}
	end := strings.Index(src[start:], "TMUX_SESSION=")
	if end < 0 {
		t.Fatal("end of the local-mode preamble not found in the Justfile")
	}
	return src[start : start+end]
}

// TestLocalModeDistinguishesConfinedAndUnconfinedBackends pins both messages.
// A blanket warning would now lie about Claude and Codex; a blanket success
// message would hide the unchanged exposure of the other backends.
func TestLocalModeDistinguishesConfinedAndUnconfinedBackends(t *testing.T) {
	block := contributeHiveLocalBranch(t)

	for _, want := range []string{
		"claude|litellm", "codex)", `_LOCAL_POSTURE="sandboxed"`,
		"workspace write confinement is enabled", "NOT confined",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("local-mode posture branch does not contain %q", want)
		}
	}
	// The warning has to name the way out, or it is an alarm rather than
	// guidance. Container mode is the default and is the remedy on this path.
	if !strings.Contains(block, "just contribute-hive ${BACKEND}") {
		t.Error("the local-mode warning does not point at container mode as the confined alternative")
	}
}

// TestLocalModeMessagesAreHonestAboutWhatStillHolds pins the hard-fail promise
// on the confined branch and the remaining controls on the fallback branch.
func TestLocalModeMessagesAreHonestAboutWhatStillHolds(t *testing.T) {
	block := contributeHiveLocalBranch(t)

	for _, want := range []string{
		"startup fails rather than", "falling back unconfined",
		"Still constrained", "host-state commands", "GitHub token",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the local-mode posture message does not mention %q", want)
		}
	}
}

func claudeLocalSandboxArgs(t *testing.T, extraEnv ...string) []string {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "workspace with spaces")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `
source ../../../config/backends.conf
eval "set -- $(claude_family_local_perm_flag_shell)"
printf '%s\0' "$@"
`)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "HIVE_WORKSPACE_DIR=") || strings.HasPrefix(entry, "HIVE_CLAUDE_DANGEROUSLY_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, append([]string{"HIVE_WORKSPACE_DIR=" + workspace}, extraEnv...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve Claude local sandbox argv: %v", err)
	}
	parts := strings.Split(string(out), "\x00")
	return parts[:len(parts)-1]
}

func argValue(args []string, key string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key {
			return args[i+1], true
		}
	}
	return "", false
}

// TestClaudeLocalSandboxIsMandatory asserts the three controls that turn the
// native sandbox into a boundary instead of a best-effort hint.
func TestClaudeLocalSandboxIsMandatory(t *testing.T) {
	args := claudeLocalSandboxArgs(t)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "dangerously-skip-permissions") {
		t.Fatalf("default local Claude argv still bypasses permissions: %q", args)
	}
	if mode, ok := argValue(args, "--permission-mode"); !ok || mode != "dontAsk" {
		t.Fatalf("permission mode = %q, %v; want dontAsk", mode, ok)
	}
	workspace, ok := argValue(args, "--add-dir")
	if !ok || !strings.Contains(workspace, "workspace with spaces") {
		t.Fatalf("workspace grant was lost or word-split: %q", args)
	}

	raw, ok := argValue(args, "--settings")
	if !ok {
		t.Fatalf("Claude local argv has no sandbox settings: %q", args)
	}
	var settings struct {
		Sandbox struct {
			Enabled                  bool `json:"enabled"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Filesystem               struct {
				AllowWrite []string `json:"allowWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings argument is not JSON: %v\n%s", err, raw)
	}
	if !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable || settings.Sandbox.AllowUnsandboxedCommands {
		t.Fatalf("sandbox is not mandatory: %+v", settings.Sandbox)
	}
	if len(settings.Sandbox.Filesystem.AllowWrite) != 1 || settings.Sandbox.Filesystem.AllowWrite[0] != workspace {
		t.Fatalf("sandbox write roots = %q, want only %q", settings.Sandbox.Filesystem.AllowWrite, workspace)
	}
}

func TestClaudeLocalSandboxNeedsExplicitDangerousBypass(t *testing.T) {
	args := claudeLocalSandboxArgs(t, "HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "dangerously-skip-permissions") || strings.Contains(joined, "--settings") {
		t.Fatalf("explicit dangerous bypass did not restore the old posture: %q", args)
	}
}

func TestClaudeHostStateOptOutDoesNotDisableLocalSandbox(t *testing.T) {
	args := claudeLocalSandboxArgs(t, "HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE=1")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--settings") || strings.Contains(joined, "dangerously-skip-permissions") {
		t.Fatalf("host-state opt-out crossed the filesystem boundary: %q", args)
	}
	if strings.Contains(joined, "Bash(rpm-ostree:*)") {
		t.Fatalf("host-state opt-out did not remove the command list: %q", args)
	}
}

// TestContainerModeRemainsTheDefault. The whole remedy above is "drop the word
// local", which only works while container mode is what you get by default.
func TestContainerModeRemainsTheDefault(t *testing.T) {
	src := justfileSource(t)
	if !strings.Contains(src, `contribute-hive backend="" mode="docker":`) {
		t.Error("contribute-hive no longer defaults to container mode; the #4918 warning's advice is stale")
	}
}
