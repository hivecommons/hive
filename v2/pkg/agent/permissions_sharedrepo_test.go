package agent

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// mkRepoDir creates a git working tree (a dir containing a .git entry) at
// parent/name and returns its path. Mode 0755 mirrors how the clone lands in
// production: dev:node but without the group-write bit agents need.
func mkRepoDir(t *testing.T, parent, name string) string {
	t.Helper()
	repo := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo %s: %v", repo, err)
	}
	if err := os.Chmod(repo, 0o755); err != nil {
		t.Fatalf("chmod repo %s: %v", repo, err)
	}
	return repo
}

// TestSharedRepoClonesFindsOnlyGitTrees pins the discovery contract: only real
// subdirectories that contain a .git are returned; dotdirs (covered by
// WatchedHomeDirs) and non-repo plain dirs are skipped. This is the set the
// watcher widens so agent UIDs can commit and open PRs.
func TestSharedRepoClonesFindsOnlyGitTrees(t *testing.T) {
	root := t.TempDir()
	origParent := SharedRepoParent
	SharedRepoParent = root
	t.Cleanup(func() { SharedRepoParent = origParent })

	wantA := mkRepoDir(t, root, "api-server")
	wantB := mkRepoDir(t, root, "ui")

	// Decoys that must NOT be returned:
	if err := os.MkdirAll(filepath.Join(root, ".cache", "sub"), 0o755); err != nil {
		t.Fatal(err) // dotdir — covered by WatchedHomeDirs, not here
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err) // plain dir, no .git
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err) // plain file
	}

	got := sharedRepoClones()
	sort.Strings(got)
	want := []string{wantA, wantB}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("sharedRepoClones() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sharedRepoClones()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSharedRepoClonesRejectsSymlinks pins the CWE-59 guard on the discovery
// path: a symlinked child (even one whose target is a real git repo) must NOT be
// returned, because the widening walk would otherwise follow it out of the shared
// HOME. This mirrors the fixEntry symlink guard for the new scan.
func TestSharedRepoClonesRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	origParent := SharedRepoParent
	SharedRepoParent = root
	t.Cleanup(func() { SharedRepoParent = origParent })

	// A real repo OUTSIDE the parent, then a symlink to it inside the parent.
	outside := t.TempDir()
	realRepo := mkRepoDir(t, outside, "api-server")
	link := filepath.Join(root, "api-server")
	if err := os.Symlink(realRepo, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Also plant a repo whose .git is itself a symlink — must be skipped too.
	sneaky := filepath.Join(root, "ui")
	if err := os.MkdirAll(sneaky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "api-server", ".git"), filepath.Join(sneaky, ".git")); err != nil {
		t.Fatalf("symlink .git: %v", err)
	}

	if got := sharedRepoClones(); len(got) != 0 {
		t.Errorf("sharedRepoClones() = %v, want empty: symlinked entries must be skipped (CWE-59)", got)
	}
}

// TestSharedRepoClonesMissingParentIsSafe verifies the best-effort contract: an
// absent parent yields nil, never an error or panic (the watcher must never
// abort a tick).
func TestSharedRepoClonesMissingParentIsSafe(t *testing.T) {
	origParent := SharedRepoParent
	SharedRepoParent = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { SharedRepoParent = origParent })

	if got := sharedRepoClones(); got != nil {
		t.Errorf("sharedRepoClones() on missing parent = %v, want nil", got)
	}
}

// TestFixPermissionsWidensSharedRepoClone is the end-to-end assertion that the
// shared repo clone — created 0755 like production — is brought to at least
// group-writable (>=0770) so an agent UID in the node group can write, commit
// and push. It only runs as DevUID because fixEntry's corrective arm is gated to
// entries the watcher owns (production: the clone is dev:node); unprivileged CI
// exercises the discovery tests above instead.
func TestFixPermissionsWidensSharedRepoClone(t *testing.T) {
	if !runningAsDevUID() {
		t.Skip("fixEntry only widens DevUID-owned entries; this process is not DevUID")
	}
	root := t.TempDir()
	origParent := SharedRepoParent
	origWatched := WatchedHomeDirs
	SharedRepoParent = root
	WatchedHomeDirs = nil // isolate: only the shared-repo scan should act
	t.Cleanup(func() {
		SharedRepoParent = origParent
		WatchedHomeDirs = origWatched
	})

	repo := mkRepoDir(t, root, "api-server")
	// A tracked file inside, group-unwritable, mirroring a checked-out source file.
	srcDir := filepath.Join(repo, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "config.ts")
	if err := os.WriteFile(srcFile, []byte("export {}"), 0o600); err != nil {
		t.Fatal(err)
	}

	fixPermissions(quietLogger())

	if got := statMode(t, repo).Perm(); got&0o070 != 0o070 {
		t.Errorf("repo dir mode = %v, want group rwx (>=0770) so agents can write", got)
	}
	if got := statMode(t, srcDir).Perm(); got&0o070 != 0o070 {
		t.Errorf("repo src dir mode = %v, want group rwx", got)
	}
	if got := statMode(t, srcFile).Perm(); got&0o060 != 0o060 {
		t.Errorf("repo file mode = %v, want group rw so agents can rewrite it", got)
	}
}
