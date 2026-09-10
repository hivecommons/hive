package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// mismatchUID is a UID the test process does not own, so os.Chown fails with
// EPERM when running unprivileged — the "chown unavailable" fallback branch.
const mismatchUID = 65432

func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("running as root: chown succeeds and permission bits are ignored")
	}
}

// restoreMode reverts a permission tweak so TempDir cleanup can remove the tree.
func restoreMode(t *testing.T, path string) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

func TestPerAgentXDGHome_NoUID_NotExported(t *testing.T) {
	withSharedAgentHome(t)
	if home, ok := perAgentXDGHome("scanner", 0, "claude"); ok || home != "" {
		t.Fatalf("uid 0: got (%q, %v), want (\"\", false)", home, ok)
	}
}

// --- tightenInteractiveHome fallback branches ---------------------------------

func TestTightenInteractiveHome_ChownUnavailable_FallsBackToSharedMode(t *testing.T) {
	requireUnprivileged(t)
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	home := filepath.Join(t.TempDir(), "agent-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	m.tightenInteractiveHome("scanner", home, mismatchUID)

	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != interactiveHomeSharedDirMode {
		t.Fatalf("mode after chown fallback = %v, want %v", got, os.FileMode(interactiveHomeSharedDirMode))
	}
}

func TestTightenInteractiveHome_MissingHome_Refuses(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	home := filepath.Join(t.TempDir(), "never-created")
	m.tightenInteractiveHome("scanner", home, mismatchUID)
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("tighten must not create the home (err=%v)", err)
	}
}

func TestTightenInteractiveHome_NoUID_NoOp(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	home := t.TempDir()
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}
	m.tightenInteractiveHome("scanner", home, 0)
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("uid 0 must leave the mode alone, got %v", got)
	}
}

// --- ownAgentDir fallback branches --------------------------------------------

func TestOwnAgentDir_ChownUnavailable_FallsBackToSharedMode(t *testing.T) {
	requireUnprivileged(t)
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	dir := filepath.Join(t.TempDir(), "xdg-data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m.ownAgentDir("scanner", dir, mismatchUID)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != interactiveHomeSharedDirMode {
		t.Fatalf("mode after chown fallback = %v, want %v", got, os.FileMode(interactiveHomeSharedDirMode))
	}
}

func TestOwnAgentDir_NoUID_NoOp(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	m.ownAgentDir("scanner", dir, 0)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("uid 0 must leave the mode alone, got %v", got)
	}
}

// --- bridgeHomeEntry error branches --------------------------------------------

func TestBridgeHomeEntry_StaleLinkRemoveFails_LinkRetained(t *testing.T) {
	requireUnprivileged(t)
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config")
	stale := filepath.Join(shared, "elsewhere")
	if err := os.Symlink(stale, link); err != nil {
		t.Fatal(err)
	}
	// Read-only parent: os.Remove on the stale link must fail.
	if err := os.Chmod(home, 0o555); err != nil {
		t.Fatal(err)
	}
	restoreMode(t, home)

	m.bridgeHomeEntry("scanner", link, filepath.Join(shared, ".config"))

	got, err := os.Readlink(link)
	if err != nil || got != stale {
		t.Fatalf("stale link must survive a failed replace: got %q err=%v", got, err)
	}
}

func TestBridgeHomeEntry_SymlinkCreateFails_Logged(t *testing.T) {
	requireUnprivileged(t)
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o555); err != nil {
		t.Fatal(err)
	}
	restoreMode(t, home)
	link := filepath.Join(home, ".config")

	m.bridgeHomeEntry("scanner", link, filepath.Join(shared, ".config"))

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("no link should exist after a failed create (err=%v)", err)
	}
}

func TestBridgeHomeEntry_LstatFails_Refuses(t *testing.T) {
	requireUnprivileged(t)
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	// A parent without the execute bit makes Lstat on entries below it fail
	// with EACCES — the "failed to inspect" branch, not IsNotExist.
	parent := filepath.Join(t.TempDir(), "opaque")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "child", ".config")
	if err := os.Mkdir(filepath.Join(parent, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o644); err != nil {
		t.Fatal(err)
	}
	restoreMode(t, parent)

	m.bridgeHomeEntry("scanner", link, filepath.Join(shared, ".config"))
	// Nothing to assert on disk (the tree is unreadable); reaching here without
	// a panic means the inspect-failure branch returned early as designed.
}

// --- seedClaudeSessionForAgent write-failure branch -----------------------------

func TestSeedClaudeSession_WriteFails_Logged(t *testing.T) {
	requireUnprivileged(t)
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	// Signed-in legacy source to adopt.
	writeSession(t, shared, signedInClaudeSession)

	// Read-only target home: WriteFile fails; UID 0 means no su-exec spec, so
	// the failure surfaces as the "failed to seed claude session" warn branch.
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o555); err != nil {
		t.Fatal(err)
	}
	restoreMode(t, home)

	ap := &AgentProcess{Name: "scanner", UID: 0, Config: config.AgentConfig{Backend: "claude"}}
	m.seedClaudeSessionForAgent(ap, home)

	if _, err := os.Lstat(claudeSessionFile(home)); !os.IsNotExist(err) {
		t.Fatalf("no session file should exist after a failed seed (err=%v)", err)
	}
}

// --- findSignedInClaudeSession / sweep ReadDir-failure branches ------------------

func TestFindSignedInClaudeSession_NoAgentsRoot_ReturnsEmpty(t *testing.T) {
	withSharedAgentHome(t) // temp dir exists, but agents/ subdir does not
	m := interactiveHomeTestManager(t)
	if got := m.findSignedInClaudeSession("scanner"); got != "" {
		t.Fatalf("want no source when the agents root is missing, got %q", got)
	}
}

func TestSweepOrphanedClaudeTmp_MissingSharedHome_NoOp(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	// Point the shared home at a path that does not exist.
	old := sharedAgentHome
	sharedAgentHome = filepath.Join(old, "never-created")
	t.Cleanup(func() { sharedAgentHome = old })
	m.sweepOrphanedClaudeTmp("scanner") // must not panic; ReadDir error branch
}
