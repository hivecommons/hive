package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the two su-exec-backed helpers in manager.go that were at
// 0%/33% coverage: writeFileAsUser and (*Manager).healForeignCodexConfig.
// Both shell out to the su-exec binary, so each test installs a stub su-exec
// at the front of PATH — the same seam TestEnsureTmuxSession_IncludesStderr
// already uses — keeping everything hermetic (no real user switching).

// installSuExecScript writes a custom su-exec stub into the stub bin dir
// already on PATH (see TestMain), mirroring installSuExecStub in
// relaunch_mint_test.go but with a caller-supplied script so failure modes
// can be simulated. The script receives su-exec's argv (userSpec cmd args...)
// verbatim.
func installSuExecScript(t *testing.T, script string) {
	t.Helper()
	p := filepath.Join(stubBinDir, "su-exec")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("writing su-exec stub: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
}

// passthroughSuExec drops the userSpec argument and executes the wrapped
// command as the current user — the hermetic stand-in for a real su-exec.
const passthroughSuExec = `#!/bin/sh
shift
exec "$@"
`

func TestWriteFileAsUser_WritesContent(t *testing.T) {
	installSuExecScript(t, passthroughSuExec)

	// A path with spaces and a single quote proves the `sh -c 'cat > "$1"'`
	// form keeps the path out of shell parsing entirely, as documented.
	dir := t.TempDir()
	path := filepath.Join(dir, `it's a spaced file.json`)
	content := []byte("line one\nline two\n")

	if err := writeFileAsUser("1234:1000", path, content); err != nil {
		t.Fatalf("writeFileAsUser: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back %s: %v", path, err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q want %q", got, content)
	}
}

func TestWriteFileAsUser_ErrorIncludesContext(t *testing.T) {
	// A failing su-exec must surface prefix, error, and captured output via
	// outputErr so the operator log names the path, the user, and the cause.
	installSuExecScript(t, `#!/bin/sh
echo "su-exec: getpwnam(hive-ghost): Success" >&2
exit 1
`)

	err := writeFileAsUser("hive-ghost", "/nonexistent/target", []byte("x"))
	if err == nil {
		t.Fatal("expected error from failing su-exec, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"writing /nonexistent/target as hive-ghost", "getpwnam(hive-ghost)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// healManager builds the minimal Manager the heal path needs (just a logger).
func healManager() *Manager {
	return &Manager{logger: discardLogger()}
}

func TestHealForeignCodexConfig_AbsentConfigIsNoop(t *testing.T) {
	m := healManager()
	dir := t.TempDir() // no config.toml inside
	agent := &AgentProcess{Name: "scout", UID: os.Getuid()}

	if got := m.healForeignCodexConfig(agent, dir, "hive-scout"); got != nil {
		t.Errorf("absent config.toml must heal to nil, got %q", got)
	}
}

func TestHealForeignCodexConfig_OwnConfigLeftAlone(t *testing.T) {
	m := healManager()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("model = \"o4\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The agent "owns" the file (its UID is ours), so nothing is foreign.
	agent := &AgentProcess{Name: "scout", UID: os.Getuid()}

	if got := m.healForeignCodexConfig(agent, dir, "hive-scout"); got != nil {
		t.Errorf("agent-owned config.toml must heal to nil, got %q", got)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("agent-owned config.toml must not be removed: %v", err)
	}
}

func TestHealForeignCodexConfig_SalvagesAndRemovesForeignConfig(t *testing.T) {
	installSuExecScript(t, passthroughSuExec)

	m := healManager()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := "model = \"operator-authored\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// The file is owned by the test uid; give the agent a DIFFERENT uid so
	// the config counts as foreign without needing a privileged chown.
	agent := &AgentProcess{Name: "scout", UID: os.Getuid() + 1}

	got := m.healForeignCodexConfig(agent, dir, "hive-scout")
	if string(got) != content {
		t.Errorf("salvaged content: got %q want %q", got, content)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("foreign config.toml must be removed, stat err = %v", err)
	}
}

func TestHealForeignCodexConfig_RemoveFailureReturnsNil(t *testing.T) {
	// su-exec refuses: the heal must give up (nil) and leave the file for the
	// documented manual fix rather than pretending it salvaged anything.
	installSuExecScript(t, `#!/bin/sh
exit 1
`)

	m := healManager()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &AgentProcess{Name: "scout", UID: os.Getuid() + 1}

	if got := m.healForeignCodexConfig(agent, dir, "hive-scout"); got != nil {
		t.Errorf("failed removal must return nil, got %q", got)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("file must survive a failed removal: %v", err)
	}
}

func TestHealForeignCodexConfig_UnreadableForeignConfigRemovedWithoutSalvage(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read 0o000 files; the unreadable branch needs an unprivileged uid")
	}
	installSuExecScript(t, passthroughSuExec)

	m := healManager()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, 0o000); err != nil {
		t.Fatal(err)
	}
	agent := &AgentProcess{Name: "scout", UID: os.Getuid() + 1}

	got := m.healForeignCodexConfig(agent, dir, "hive-scout")
	if got != nil {
		t.Errorf("unreadable config must salvage nothing, got %q", got)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("unreadable foreign config must still be removed, stat err = %v", err)
	}
}
