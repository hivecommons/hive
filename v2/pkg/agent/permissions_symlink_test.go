package agent

import (
beads.json "os"
beads.json "path/filepath"
beads.json "testing"
)

// TestFixEntrySkipsSymlinks pins the CWE-59 guard added in PR #3432: a planted
// symlink in the agent HOME tree must never be followed, because both os.Chmod
// and os.Chown follow symlinks — following one would redirect the repair loop
// onto a file outside the watched tree, allowing a local attacker to escalate
// permissions on an arbitrary path.
//
// filepath.Walk reports symlinks via Lstat, so fi.Mode() has ModeSymlink set,
// and fixEntry must return immediately without acting.
func TestFixEntrySkipsSymlinks(t *testing.T) {
beads.json root := t.TempDir()

beads.json // Create a target file with tight permissions that must NOT be widened.
beads.json target := filepath.Join(root, "outside-target")
beads.json if err := os.WriteFile(target, []byte("sensitive"), 0o600); err != nil {
beads.json beads.json t.Fatalf("write target: %v", err)
beads.json }
beads.json if err := os.Chmod(target, 0o600); err != nil {
beads.json beads.json t.Fatalf("chmod target: %v", err)
beads.json }

beads.json // Plant a symlink inside the "watched tree" pointing at the target.
beads.json link := filepath.Join(root, "planted-link")
beads.json if err := os.Symlink(target, link); err != nil {
beads.json beads.json t.Fatalf("symlink: %v", err)
beads.json }

beads.json // Lstat the link — this is what filepath.Walk provides to its WalkFunc.
beads.json fi, err := os.Lstat(link)
beads.json if err != nil {
beads.json beads.json t.Fatalf("lstat: %v", err)
beads.json }
beads.json if fi.Mode()&os.ModeSymlink == 0 {
beads.json beads.json t.Fatal("precondition: Lstat did not report ModeSymlink")
beads.json }

beads.json // Call fixEntry on the symlink — it must be a no-op.
beads.json fixEntry(link, fi, quietLogger())

beads.json // The TARGET's permissions must be unchanged — proof the link was not followed.
beads.json after, err := os.Stat(target)
beads.json if err != nil {
beads.json beads.json t.Fatalf("stat target after fixEntry: %v", err)
beads.json }
beads.json if got := after.Mode().Perm(); got != 0o600 {
beads.json beads.json t.Errorf("target mode = %v after fixEntry on a symlink — the symlink was followed (CWE-59 regression), want 0600", got)
beads.json }
}

// TestFixPermissionsDoesNotFollowSymlinksInWalk exercises the full
// fixPermissions walk with a symlink planted inside a watched root. The
// symlink's target must not have its permissions altered.
func TestFixPermissionsDoesNotFollowSymlinksInWalk(t *testing.T) {
beads.json root := t.TempDir()
beads.json watched := filepath.Join(root, "watched")
beads.json if err := os.MkdirAll(watched, 0o755); err != nil {
beads.json beads.json t.Fatalf("mkdir watched: %v", err)
beads.json }

beads.json // Target outside the watched tree.
beads.json target := filepath.Join(root, "outside-secret")
beads.json if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
beads.json beads.json t.Fatalf("write target: %v", err)
beads.json }
beads.json if err := os.Chmod(target, 0o600); err != nil {
beads.json beads.json t.Fatalf("chmod target: %v", err)
beads.json }

beads.json // Symlink inside the watched tree pointing outside.
beads.json link := filepath.Join(watched, "evil-link")
beads.json if err := os.Symlink(target, link); err != nil {
beads.json beads.json t.Fatalf("symlink: %v", err)
beads.json }

beads.json // Point the watcher at our temp tree.
beads.json origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
beads.json WatchedHomeDirs = []string{watched}
beads.json GooseLogsDir = filepath.Join(root, "goose-logs")
beads.json t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

beads.json fixPermissions(quietLogger())

beads.json // Assert the target was not touched.
beads.json after, err := os.Stat(target)
beads.json if err != nil {
beads.json beads.json t.Fatalf("stat target: %v", err)
beads.json }
beads.json if got := after.Mode().Perm(); got != 0o600 {
beads.json beads.json t.Errorf("target mode = %v after walk containing a symlink — CWE-59 regression, want 0600", got)
beads.json }
}
