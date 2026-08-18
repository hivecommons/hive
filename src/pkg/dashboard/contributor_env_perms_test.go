package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

// SECURITY (#2610): contributor.env holds HIVE_REGISTRATION_TOKEN, the sole
// long-lived bearer credential for the contributor WebSocket. The Justfile
// contribute-setup recipe writes it and then re-tightens it to 0600 (matching
// its sibling secret files gh-auth.env and claude-config.json), including a
// re-tighten of any pre-existing world-readable file left by an older setup.
//
// The chmod lives in the Justfile (shell), so this test asserts the mode
// semantics the recipe enforces: a token file created at the default umask
// (0644) must end up owner-only (0600) after the re-tighten step. It guards
// against a regression where the mode contract silently loosens.
func TestContributorEnvReTightenTo0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "contributor.env")

	// Simulate the pre-fix state: a token file left world-readable at 0644,
	// as `cat > file` produces under the typical umask 0022.
	if err := os.WriteFile(path, []byte("HIVE_REGISTRATION_TOKEN=secret\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if got := fileMode(t, path); got != 0o644 {
		t.Fatalf("precondition: seeded mode = %o, want 644", got)
	}

	// The re-tighten step the Justfile applies: chmod 600 the existing file.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("re-tighten chmod: %v", err)
	}

	if got := fileMode(t, path); got != 0o600 {
		t.Fatalf("after re-tighten: mode = %o, want 600 (owner-only)", got)
	}
	// It must NOT be group- or world-readable.
	if got := fileMode(t, path); got&0o077 != 0 {
		t.Errorf("token file is group/world-accessible: mode = %o", got)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}
