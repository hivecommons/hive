package sessionprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneClaudeTmpRemovesOnlyIdleSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)

	project := filepath.Join(root, "claude-2003", "-data-agents-scanner")
	idle := filepath.Join(project, "f0a329e7-1341-4cff-82a0-3276b8fbc52c")
	live := filepath.Join(project, "9c242e27-ac82-48c9-b1f2-9885910b15af")
	mkTree(t, idle, old)
	mkTree(t, live, old)
	// A live session writes deep inside an otherwise old tree: the newest
	// write anywhere inside decides, not the directory's own mtime.
	mkTree(t, filepath.Join(live, "scratchpad", "repo"), recent)

	// Outside claude-*/<project>/<session>: never touched, however old.
	other := filepath.Join(root, "gos-r")
	mkTree(t, other, old)
	shallow := filepath.Join(root, "claude-2003", "loose-file-dir")
	mkTree(t, shallow, old)

	res, err := PruneClaudeTmp(root, 24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneClaudeTmp: %v", err)
	}
	if res.Removed != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want 1 removed", res)
	}
	if _, err := os.Stat(idle); !os.IsNotExist(err) {
		t.Errorf("idle session still present: %v", err)
	}
	for _, keep := range []string{live, other, shallow, project} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s removed: %v", keep, err)
		}
	}
}

func TestPruneClaudeTmpNeverFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)

	target := filepath.Join(t.TempDir(), "precious")
	mkTree(t, target, old)
	project := filepath.Join(root, "claude-1000", "-data-agents-quality")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(project, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, filepath.Join(root, "claude-9999")); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneClaudeTmp(root, 24*time.Hour, now, quietLogger()); err != nil {
		t.Fatalf("PruneClaudeTmp: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "data")); err != nil {
		t.Fatalf("symlink target was pruned: %v", err)
	}
}

func TestPruneClaudeTmpNoOps(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		root   string
		maxAge time.Duration
	}{
		{"", time.Hour},
		{filepath.Join(t.TempDir(), "missing"), time.Hour},
		{t.TempDir(), 0},
	} {
		res, err := PruneClaudeTmp(tc.root, tc.maxAge, now, quietLogger())
		if err != nil || res != (Result{}) {
			t.Errorf("PruneClaudeTmp(%q, %v) = %+v, %v; want no-op", tc.root, tc.maxAge, res, err)
		}
	}
}
