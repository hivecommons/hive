package reach

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs one git command against dir and fails the test on error —
// the fixture-repo pattern pkg/policies' watcher_git_test.go uses.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// gitCommit writes a file and commits it, returning the new commit SHA.
func gitCommit(t *testing.T, dir, file, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", msg)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// newFixtureRepo builds a real repo in t.TempDir() with this graph:
//
//	c1 — c2 — c3          (main line: c3 descends from c2 descends from c1)
//	  \
//	   side               (side branch off c1: NOT an ancestor of c3)
//
// and returns the four SHAs.
func newFixtureRepo(t *testing.T) (dir, c1, c2, c3, side string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	dir = t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "test")

	c1 = gitCommit(t, dir, "a.txt", "one", "c1")
	c2 = gitCommit(t, dir, "a.txt", "two", "c2")
	c3 = gitCommit(t, dir, "a.txt", "three", "c3")

	gitRun(t, dir, "checkout", "-b", "side", c1)
	side = gitCommit(t, dir, "b.txt", "side", "side")
	return dir, c1, c2, c3, side
}

// TestGitAncestry exercises the ancestry join's primitive against a REAL
// commit graph: descendants qualify, ancestors and diverged branches do not,
// a commit is its own ancestor, and unknown SHAs are errors — never a
// silent false/true.
func TestGitAncestry(t *testing.T) {
	dir, c1, c2, c3, side := newFixtureRepo(t)
	anc := NewGitAncestry(dir)

	cases := []struct {
		name                 string
		ancestor, descendant string
		want                 bool
	}{
		{"direct parent", c2, c3, true},
		{"grandparent", c1, c3, true},
		{"self", c2, c2, true},
		{"reversed order", c3, c1, false},
		{"diverged branch", c2, side, false},
		{"branch base still counts", c1, side, true},
	}
	for _, tc := range cases {
		got, err := anc.IsAncestor(tc.ancestor, tc.descendant)
		if err != nil {
			t.Fatalf("%s: IsAncestor: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: IsAncestor = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Cached second call must agree (answers are immutable).
	again, err := anc.IsAncestor(c1, c3)
	if err != nil || !again {
		t.Errorf("cached IsAncestor(c1,c3) = %v, %v; want true, nil", again, err)
	}

	// Unknown SHA and empty input are ERRORS, not answers.
	if _, err := anc.IsAncestor("0000000000000000000000000000000000000000", c3); err == nil {
		t.Error("unknown SHA: want error, got nil")
	}
	if _, err := anc.IsAncestor("", c3); err == nil {
		t.Error("empty ancestor: want error, got nil")
	}
}
