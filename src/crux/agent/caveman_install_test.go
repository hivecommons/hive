package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCavemanInstallArgvRunsAsAgentUser pins the fix for the caveman clobber:
// the installer spawns the backend's own CLI (`claude plugin list` / `plugin
// install`), and Claude Code rewrites $HOME/.claude.json wholesale on every
// invocation. Run as the hive user with the agent's HOME, that CLI cannot
// read the agent-owned 0600 session file, treats the home as a fresh
// install, and replaces the signed-in session with a blank skeleton — the
// agent drops to the login menu on its next start (#4596 reintroduced from
// inside the manager, through the per-agent layout #4619 built to end it).
// Every per-UID install must therefore be wrapped in su-exec, exactly like
// tmuxCmd and setupCodexHome.
func TestCavemanInstallArgvRunsAsAgentUser(t *testing.T) {
	backends := []string{"claude", "copilot", "gemini", "goose", "codex", "aider"}
	for _, backend := range backends {
		argv := cavemanInstallArgv(backend, "full", "hive-scanner")
		if len(argv) < 3 || argv[0] != "su-exec" || argv[1] != "hive-scanner" {
			t.Errorf("backend %s: per-UID install must run via su-exec as the agent user, got %v", backend, argv)
			continue
		}
		if argv[2] != "npx" {
			t.Errorf("backend %s: expected npx after the su-exec prefix, got %v", backend, argv)
		}
	}
}

// TestCavemanInstallArgvSharedUIDStaysDirect pins the UID==0 path: agents
// without an allocated UID run as the hive user, so there is no user to
// switch to and no su-exec prefix (su-exec may not even exist on such
// deployments).
func TestCavemanInstallArgvSharedUIDStaysDirect(t *testing.T) {
	argv := cavemanInstallArgv("claude", "minimal", "")
	if len(argv) == 0 || argv[0] != "npx" {
		t.Errorf("shared-UID install must invoke npx directly, got %v", argv)
	}
	for _, a := range argv {
		if a == "su-exec" {
			t.Errorf("shared-UID install must not use su-exec: %v", argv)
		}
	}
}

func TestCavemanInstallArgvUnsupportedBackend(t *testing.T) {
	if argv := cavemanInstallArgv("mystery", "full", "hive-x"); argv != nil {
		t.Errorf("unsupported backend must return nil, got %v", argv)
	}
}

// TestCavemanNpmCachePathReplacesForeignOwnedCache covers the migration
// burn: caches written back when the install ran as the hive user are owned
// by the hive UID, and npm run as the agent UID EACCESes on those shards
// (the same failure class as #2284). A cache owned by another UID must be
// removed so the agent-owned install starts clean.
func TestCavemanNpmCachePathReplacesForeignOwnedCache(t *testing.T) {
	agentDir := t.TempDir()
	cache := filepath.Join(agentDir, ".npm-caveman-cache")
	if err := os.MkdirAll(filepath.Join(cache, "_cacache"), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}

	// Any UID other than the one that owns the dir we just made.
	foreignUID := os.Getuid() + 1
	got := cavemanNpmCachePath(agentDir, foreignUID)
	if got != cache {
		t.Errorf("removable foreign cache should be replaced in place, got %q", got)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("foreign-owned cache should have been removed, stat err = %v", err)
	}
}

func TestCavemanNpmCachePathKeepsOwnCache(t *testing.T) {
	agentDir := t.TempDir()
	cache := filepath.Join(agentDir, ".npm-caveman-cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if got := cavemanNpmCachePath(agentDir, os.Getuid()); got != cache {
		t.Errorf("cache already owned by the agent must be kept, got %q", got)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("agent-owned cache must not be removed: %v", err)
	}
}

// TestCavemanNpmCachePathFallsBackWhenRemovalFails pins the last-resort
// path: a foreign-owned cache that cannot be removed is sidestepped with a
// per-UID dir rather than allowed to fail the install with EACCES.
func TestCavemanNpmCachePathFallsBackWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — removal cannot be made to fail with permissions")
	}
	agentDir := t.TempDir()
	cache := filepath.Join(agentDir, ".npm-caveman-cache")
	if err := os.MkdirAll(filepath.Join(cache, "_cacache"), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	// Read-only parent makes the unlink inside RemoveAll fail.
	if err := os.Chmod(agentDir, 0o500); err != nil {
		t.Fatalf("chmod agent dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentDir, 0o755) })

	foreignUID := os.Getuid() + 1
	got := cavemanNpmCachePath(agentDir, foreignUID)
	want := fmt.Sprintf("%s-u%d", cache, foreignUID)
	if got != want {
		t.Errorf("unremovable foreign cache must fall back to a per-UID path: got %q want %q", got, want)
	}
}
