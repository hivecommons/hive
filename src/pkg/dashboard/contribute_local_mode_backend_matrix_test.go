package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #4918 remaining-backend confinement. #5011 closed the gap for claude and
// litellm only, via Claude Code's native sandbox. This file pins the rest of
// the backend matrix:
//
//   - copilot: has its own OS-enforced sandbox (--sandbox, MXC: Seatbelt on
//     macOS, bubblewrap on Linux). Wired the same way the claude sandbox is —
//     gated on the installed CLI actually supporting the flag, with an
//     explicit dangerous-bypass escape hatch.
//   - opencode: has NO OS sandbox, but does have a real command-deny
//     mechanism (permission.bash pattern config) that survives --auto. Wired
//     as a floor, matching the claude host-state deny-list precedent — and
//     the local-mode banner must never call this "confined", only
//     "denylisted", because it is not a filesystem boundary.
//   - goose, agy, bob, pi, aider: verified against each CLI's own current
//     docs to have no sandbox, no filesystem allowlist, and no deny
//     mechanism at all. Local mode for these MUST refuse to launch without
//     an explicit per-backend HIVE_<BACKEND>_DANGEROUSLY_RUN_UNCONFINED=1 —
//     an honest refusal, not a silent unconfined launch.
//
// backendsConfSource/runBackendsConf below shell out to bash the same way
// contribute_local_mode_warning_test.go's claudeLocalSandboxArgs does.

func backendsConfSource(t *testing.T) string {
	t.Helper()
	return fileSource(t, "config/backends.conf")
}

// runBackendsConfFunc sources backends.conf, calls fn with args, and returns
// (stdout, exit code). A non-zero exit is a real failure mode here (the
// unconfined-backend refusal), not a test-harness error, so callers assert
// on it rather than treating it as fatal.
func runBackendsConfFunc(t *testing.T, fn string, args []string, extraEnv ...string) (string, int) {
	t.Helper()
	script := "source ../../../config/backends.conf\n" + fn
	for _, a := range args {
		script += " " + a
	}
	script += "\n"
	cmd := exec.Command("bash", "-c", script)
	for _, entry := range os.Environ() {
		skip := false
		for _, prefix := range []string{
			"HIVE_WORKSPACE_DIR=", "HIVE_COPILOT_DANGEROUSLY_",
			"HIVE_OPENCODE_DANGEROUSLY_", "HIVE_GOOSE_DANGEROUSLY_",
			"HIVE_AGY_DANGEROUSLY_", "HIVE_BOB_DANGEROUSLY_",
			"HIVE_PI_DANGEROUSLY_", "HIVE_AIDER_DANGEROUSLY_",
		} {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("running %s: %v\n%s", fn, err, out)
		}
	}
	return string(out), exitCode
}

// ── The five backends with no confinement mechanism at all ─────────────

func TestUnconfinedBackendsRefuseToLaunchByDefault(t *testing.T) {
	for _, tc := range []struct {
		backend string
		envVar  string
	}{
		{"goose", "HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED"},
		{"agy", "HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED"},
		{"bob", "HIVE_BOB_DANGEROUSLY_RUN_UNCONFINED"},
		{"pi", "HIVE_PI_DANGEROUSLY_RUN_UNCONFINED"},
		{"aider", "HIVE_AIDER_DANGEROUSLY_RUN_UNCONFINED"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			out, code := runBackendsConfFunc(t, "unconfined_local_perm_flag_shell", []string{tc.backend})
			if code == 0 {
				t.Fatalf("%s local launch did not refuse without %s; output: %q", tc.backend, tc.envVar, out)
			}
			if !strings.Contains(out, "Refusing to launch") {
				t.Errorf("%s refusal message does not say it refused to launch: %q", tc.backend, out)
			}
			if !strings.Contains(out, tc.envVar) {
				t.Errorf("%s refusal message does not name its own escape hatch %s: %q", tc.backend, tc.envVar, out)
			}
			if !strings.Contains(out, "no sandbox") {
				t.Errorf("%s refusal message does not say honestly that there is no sandbox: %q", tc.backend, out)
			}
		})
	}
}

func TestUnconfinedBackendsLaunchWithExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		backend string
		envVar  string
	}{
		{"goose", "HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED"},
		{"agy", "HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED"},
		{"bob", "HIVE_BOB_DANGEROUSLY_RUN_UNCONFINED"},
		{"pi", "HIVE_PI_DANGEROUSLY_RUN_UNCONFINED"},
		{"aider", "HIVE_AIDER_DANGEROUSLY_RUN_UNCONFINED"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			_, code := runBackendsConfFunc(t, "unconfined_local_perm_flag_shell", []string{tc.backend}, tc.envVar+"=1")
			if code != 0 {
				t.Fatalf("%s local launch still refused with %s=1 set", tc.backend, tc.envVar)
			}
		})
	}
}

// TestEveryUnconfinedBackendHasItsOwnEscapeHatch guards against a copy-paste
// slip where two backends share one env var name — that would let opting
// into one silently opt into another with no confinement either.
func TestEveryUnconfinedBackendHasItsOwnEscapeHatch(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct{ backend, envVar string }{
		{"goose", "HIVE_GOOSE_DANGEROUSLY_RUN_UNCONFINED"},
		{"agy", "HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED"},
		{"bob", "HIVE_BOB_DANGEROUSLY_RUN_UNCONFINED"},
		{"pi", "HIVE_PI_DANGEROUSLY_RUN_UNCONFINED"},
		{"aider", "HIVE_AIDER_DANGEROUSLY_RUN_UNCONFINED"},
	} {
		if prior, ok := seen[tc.envVar]; ok {
			t.Fatalf("escape hatch %s is claimed by both %s and %s", tc.envVar, prior, tc.backend)
		}
		seen[tc.envVar] = tc.backend
	}
}

// ── copilot: real OS-enforced sandbox, gated on CLI support ────────────

func TestCopilotLocalSandboxWiredWhenCLISupportsIt(t *testing.T) {
	fakeBin := t.TempDir()
	writeFakeCopilot(t, fakeBin, "--sandbox is a real flag\n--add-dir <path>\n")

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runBackendsConfFunc(t, "copilot_local_perm_flag_shell", nil,
		"PATH="+fakeBin+":"+os.Getenv("PATH"), "HIVE_WORKSPACE_DIR="+workspace)
	if code != 0 {
		t.Fatalf("copilot local sandbox wiring failed: %q", out)
	}
	if !strings.Contains(out, "--sandbox") {
		t.Fatalf("copilot local argv missing --sandbox: %q", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("unexpected fallback warning with a CLI that supports --sandbox: %q", out)
	}
}

func TestCopilotLocalSandboxFallsBackWhenCLILacksFlag(t *testing.T) {
	fakeBin := t.TempDir()
	writeFakeCopilot(t, fakeBin, "an old copilot CLI with no sandbox support\n")

	out, code := runBackendsConfFunc(t, "copilot_local_perm_flag_shell", nil,
		"PATH="+fakeBin+":"+os.Getenv("PATH"))
	if code != 0 {
		t.Fatalf("copilot fallback path should not itself fail: %q", out)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "--sandbox") {
		t.Fatalf("old-CLI fallback did not warn honestly about the missing flag: %q", out)
	}
	// The warning text itself names the missing --sandbox flag, so check the
	// EMITTED ARGV (the last line) rather than the whole output for the flag.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	argvLine := lines[len(lines)-1]
	if strings.Contains(argvLine, "--sandbox") {
		t.Fatalf("fallback argv must not claim --sandbox when the installed CLI does not support it: %q", argvLine)
	}
	if !strings.Contains(argvLine, "--allow-all") {
		t.Fatalf("fallback argv should be the plain unconfined copilot posture: %q", argvLine)
	}
}

func TestCopilotLocalSandboxExplicitBypassSkipsTheCLIProbe(t *testing.T) {
	fakeBin := t.TempDir()
	writeFakeCopilot(t, fakeBin, "an old copilot CLI with no sandbox support\n")

	out, code := runBackendsConfFunc(t, "copilot_local_perm_flag_shell", nil,
		"PATH="+fakeBin+":"+os.Getenv("PATH"), "HIVE_COPILOT_DANGEROUSLY_BYPASS_SANDBOX=1")
	if code != 0 {
		t.Fatalf("explicit bypass should not fail: %q", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("explicit bypass should not also print the unsolicited warning: %q", out)
	}
}

func writeFakeCopilot(t *testing.T, dir, helpOutput string) {
	t.Helper()
	path := filepath.Join(dir, "copilot")
	script := "#!/usr/bin/env bash\nif [[ \"$1\" == \"--help\" ]]; then cat <<'EOF'\n" + helpOutput + "EOF\nexit 0\nfi\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ── opencode: command deny-list, not a sandbox ──────────────────────────

func TestOpencodeLocalModeDeniesHostStateCommands(t *testing.T) {
	out, code := runBackendsConfFunc(t, "opencode_local_perm_flag_shell", nil)
	if code != 0 {
		t.Fatalf("opencode local deny-list wiring failed: %q", out)
	}
	if !strings.Contains(out, "OPENCODE_PERMISSION=") {
		t.Fatalf("opencode argv missing OPENCODE_PERMISSION: %q", out)
	}
	if !strings.Contains(out, "--auto") {
		t.Fatalf("opencode argv missing --auto: %q", out)
	}

	// Unquote the shell-escaped output the same way the real consumers do
	// (tmux send-keys / heredoc re-parse): `eval "set -- $out"`. Written to a
	// script file, rather than interpolated into a -c string, so the JSON's
	// own quoting cannot collide with Go's command-string construction.
	script := filepath.Join(t.TempDir(), "unquote.sh")
	if err := os.WriteFile(script, []byte("out='"+strings.ReplaceAll(out, "'", `'\''`)+"'\neval \"set -- $out\"\nprintf '%s\\0' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unquoteCmd := exec.Command("bash", script)
	raw, err := unquoteCmd.Output()
	if err != nil {
		t.Fatalf("unquote opencode local argv: %v", err)
	}
	words := strings.Split(string(raw), "\x00")
	words = words[:len(words)-1]
	var permJSON string
	for _, w := range words {
		if strings.HasPrefix(w, "OPENCODE_PERMISSION=") {
			permJSON = strings.TrimPrefix(w, "OPENCODE_PERMISSION=")
		}
	}
	if permJSON == "" {
		t.Fatalf("OPENCODE_PERMISSION assignment not found among unquoted words: %q", words)
	}
	// Confirm it actually denies the same host-state family the claude list
	// covers, and does not deny everything (which would just be a
	// differently-shaped unconfined refusal).
	var perm struct {
		Bash map[string]string `json:"bash"`
	}
	if err := json.Unmarshal([]byte(permJSON), &perm); err != nil {
		t.Fatalf("OPENCODE_PERMISSION is not valid JSON: %v\n%s", err, permJSON)
	}
	if perm.Bash["*"] != "allow" {
		t.Errorf(`expected catch-all "*" to be "allow" (deny-list, not allow-list), got %q`, perm.Bash["*"])
	}
	for _, pattern := range []string{"sudo *", "pkexec *", "rpm-ostree *", "bootc *"} {
		if perm.Bash[pattern] != "deny" {
			t.Errorf("expected opencode permission.bash to deny %q, got %q", pattern, perm.Bash[pattern])
		}
	}
}

func TestOpencodeHostStateOptOutDropsTheDenyList(t *testing.T) {
	out, code := runBackendsConfFunc(t, "opencode_local_perm_flag_shell", nil,
		"HIVE_OPENCODE_DANGEROUSLY_ALLOW_HOST_STATE=1")
	if code != 0 {
		t.Fatalf("opencode opt-out should not fail: %q", out)
	}
	if strings.Contains(out, "OPENCODE_PERMISSION=") {
		t.Fatalf("explicit host-state opt-out should drop the deny-list entirely: %q", out)
	}
}

// TestLocalModeBannerNeverCallsOpencodeConfined pins the honesty requirement
// directly: the banner text must distinguish opencode's command-deny floor
// from an actual sandbox, using the same distinct-message contract
// TestLocalModeDistinguishesConfinedAndUnconfinedBackends already pins for
// claude/codex.
func TestLocalModeBannerNeverCallsOpencodeConfined(t *testing.T) {
	block := contributeHiveLocalBranch(t)
	for _, want := range []string{
		`_LOCAL_POSTURE="denylisted"`,
		"opencode has no OS sandbox and no filesystem write-allowlist",
		"not a boundary",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("local-mode posture branch does not contain %q", want)
		}
	}
}

// TestLocalModeBannerNamesCopilotSandbox pins that copilot gets the
// "sandboxed" banner branch, backed by its own --sandbox flag, not lumped in
// with the generic unconfined warning.
func TestLocalModeBannerNamesCopilotSandbox(t *testing.T) {
	block := contributeHiveLocalBranch(t)
	for _, want := range []string{
		"copilot)",
		"HIVE_COPILOT_DANGEROUSLY_BYPASS_SANDBOX",
		"Seatbelt on macOS",
		"bubblewrap on Linux",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("local-mode posture branch does not contain %q", want)
		}
	}
}

// TestBackendsConfDocumentsWhyUnconfinedBackendsHaveNoWiring guards the honesty
// requirement at the source: the "no confinement mechanism at all" block
// must exist and must not silently shrink: every backend with no OS-level
// sandbox has to stay named there. kilo joined the list in #5081.
func TestBackendsConfDocumentsWhyUnconfinedBackendsHaveNoWiring(t *testing.T) {
	src := backendsConfSource(t)
	for _, want := range []string{
		"goose, agy, bob, pi, aider, and kilo expose no OS-level sandbox",
		"unconfined_local_backend_env_var",
		"unconfined_local_perm_flag_shell",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("config/backends.conf no longer documents %q", want)
		}
	}
}
