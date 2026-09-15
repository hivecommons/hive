package sessionprune

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mkSession creates a session directory containing an events.jsonl and sets the
// mtime of both the file and the directory to the given times.
func mkSession(t *testing.T, root, name string, dirMod, fileMod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", events, err)
	}
	if err := os.Chtimes(events, fileMod, fileMod); err != nil {
		t.Fatalf("chtimes file: %v", err)
	}
	// The directory must be stamped after its contents, otherwise writing the
	// file would bump the directory mtime back to now.
	if err := os.Chtimes(dir, dirMod, dirMod); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}
	return dir
}

func TestPruneRemovesOnlyAgedSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	aged := mkSession(t, root, "11111111-1111-4111-8111-111111111111", old, old)
	fresh := mkSession(t, root, "22222222-2222-4222-8222-222222222222", recent, recent)

	res, err := Prune(root, 7*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Errorf("aged session should have been removed, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh session should have survived: %v", err)
	}
	if res.Removed != 1 || res.Scanned != 2 || res.Failed != 0 {
		t.Errorf("got %+v, want Scanned=2 Removed=1 Failed=0", res)
	}
}

// A live session appends to events.jsonl without ever changing its directory's
// mtime. Trusting the directory mtime alone would delete an in-flight session,
// so this is the regression test that matters most.
func TestPruneKeepsSessionWithStaleDirMtimeButRecentFile(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	staleDir := now.Add(-30 * 24 * time.Hour)
	recentWrite := now.Add(-2 * time.Minute)

	live := mkSession(t, root, "33333333-3333-4333-8333-333333333333", staleDir, recentWrite)

	res, err := Prune(root, 7*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live session was deleted despite a recent write: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0", res.Removed)
	}
}

func TestPruneIgnoresNonSessionEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	// A non-UUID directory and a loose file, both aged. Neither may be touched.
	notASession := filepath.Join(root, "backups")
	if err := os.MkdirAll(notASession, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(notASession, old, old); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(root, "config.json")
	if err := os.WriteFile(loose, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(loose, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := Prune(root, 7*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(notASession); err != nil {
		t.Errorf("non-session directory was removed: %v", err)
	}
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("loose file was removed: %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want Scanned=0 Removed=0", res)
	}
}

func TestPruneMissingDirIsNotAnError(t *testing.T) {
	res, err := Prune(filepath.Join(t.TempDir(), "nope"), 7*24*time.Hour, time.Now(), quietLogger())
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want zero result", res)
	}
}

func TestPruneDisabledWhenMaxAgeNonPositive(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-365 * 24 * time.Hour)
	aged := mkSession(t, root, "44444444-4444-4444-8444-444444444444", old, old)

	for _, maxAge := range []time.Duration{0, -time.Hour} {
		res, err := Prune(root, maxAge, now, quietLogger())
		if err != nil {
			t.Fatalf("Prune(maxAge=%v): %v", maxAge, err)
		}
		if res.Removed != 0 {
			t.Errorf("maxAge=%v removed %d sessions, want 0", maxAge, res.Removed)
		}
		if _, err := os.Stat(aged); err != nil {
			t.Fatalf("maxAge=%v deleted a session while disabled: %v", maxAge, err)
		}
	}
}

// Boundary: a session exactly at the cutoff is kept. Prune only removes what is
// strictly older, so a session cannot be deleted a moment early.
func TestPruneKeepsSessionExactlyAtCutoff(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	maxAge := 7 * 24 * time.Hour
	exact := now.Add(-maxAge)

	dir := mkSession(t, root, "55555555-5555-4555-8555-555555555555", exact, exact)

	res, err := Prune(root, maxAge, now, quietLogger())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("session exactly at cutoff was removed: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0", res.Removed)
	}
}

func TestIsSessionDirName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"db82f47d-82d2-43fd-a9c8-e3b9484c9cc4", true},
		{"DB82F47D-82D2-43FD-A9C8-E3B9484C9CC4", true},
		{"db82f47d-82d2-43fd-a9c8-e3b9484c9cc", false},   // too short
		{"db82f47d-82d2-43fd-a9c8-e3b9484c9cc44", false}, // too long
		{"db82f47d_82d2_43fd_a9c8_e3b9484c9cc4", false},  // wrong separators
		{"zb82f47d-82d2-43fd-a9c8-e3b9484c9cc4", false},  // non-hex
		{"backups", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := IsSessionDirName(tc.name); got != tc.want {
			t.Errorf("IsSessionDirName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPruneReturnsReadDirErrors covers the non-ENOENT failure path: a missing
// session root is benign (see TestPruneMissingDirIsNotAnError), but any other
// failure to read it must surface rather than be reported as "nothing to do".
// Silently returning a zero Result there would let a misconfigured path look
// like a healthy no-op prune forever.
func TestPruneReturnsReadDirErrors(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := Prune(notADir, 7*24*time.Hour, time.Now(), quietLogger())
	if err == nil {
		t.Fatal("Prune on a non-directory returned nil error; want the ReadDir failure surfaced")
	}
	if res.Removed != 0 || res.Scanned != 0 {
		t.Errorf("Prune on error = %+v, want zero-valued Result", res)
	}
}

// TestPruneKeepsSessionWhenStatFails is the safety contract for an unreadable
// session: if the age of a directory cannot be determined, it must be KEPT.
// Deleting on a stat error would turn a transient NFS hiccup into data loss on
// a live session, which is the opposite of what a retention sweep should do.
func TestPruneKeepsSessionWhenStatFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}
	root := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	dir := mkSession(t, root, "11111111-2222-3333-4444-555555555555", old, old)

	// Make the session directory untraversable so WalkDir fails inside it.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	res, err := Prune(root, 7*24*time.Hour, time.Now(), quietLogger())
	if err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}
	if res.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", res.Scanned)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0 — an unstattable session must be kept", res.Removed)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("session directory was removed despite the stat failure: %v", err)
	}
}

// TestPruneCountsFailedRemovals covers the RemoveAll failure path: a session
// that is old enough to delete but cannot be deleted must be counted in Failed
// and must not inflate Removed. Removed is what the wire logs as work done, so
// counting an un-deleted directory there would report phantom progress while
// the directory keeps consuming the PVC.
func TestPruneCountsFailedRemovals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny removal")
	}
	root := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	dir := mkSession(t, root, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", old, old)

	// A read-only parent permits reading the entry but forbids unlinking it.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	res, err := Prune(root, 7*24*time.Hour, time.Now(), quietLogger())
	if err != nil {
		t.Fatalf("Prune returned error: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("Failed = %d, want 1", res.Failed)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0 — a failed removal must not count as removed", res.Removed)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("session directory reported as present but is gone: %v", err)
	}
}
