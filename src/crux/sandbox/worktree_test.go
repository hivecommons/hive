package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunStageWorktreesAreIsolatedAndCleanedUp(t *testing.T) {
	repo := initWorktreeRepo(t)
	root := t.TempDir()
	mgr := WorktreeManager{}

	first, err := mgr.Add(context.Background(), WorktreeSpec{
		SourceRepo: repo, WorkspaceDir: root, RunKey: "myorg/repo#1", Stage: "implement", Generation: 1, TargetRef: "main",
	})
	if err != nil {
		t.Fatalf("add first worktree: %v", err)
	}
	second, err := mgr.Add(context.Background(), WorktreeSpec{
		SourceRepo: repo, WorkspaceDir: root, RunKey: "myorg/repo#2", Stage: "implement", Generation: 1, TargetRef: "main",
	})
	if err != nil {
		t.Fatalf("add second worktree: %v", err)
	}
	if first.Path == second.Path {
		t.Fatal("two run stages reused the same worktree")
	}
	if err := os.WriteFile(filepath.Join(first.Path, "first.txt"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second.Path, "second.txt"), []byte("two"), 0o600); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if status := gitOut(t, first.Path, "status", "--porcelain"); !strings.Contains(status, "first.txt") || strings.Contains(status, "second.txt") {
		t.Fatalf("first status not isolated: %q", status)
	}
	if status := gitOut(t, second.Path, "status", "--porcelain"); !strings.Contains(status, "second.txt") || strings.Contains(status, "first.txt") {
		t.Fatalf("second status not isolated: %q", status)
	}

	if err := mgr.Remove(context.Background(), repo, first.Path); err != nil {
		t.Fatalf("remove first worktree: %v", err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("first worktree still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("second worktree should remain live: %v", err)
	}
}

func TestRunStageWorktreeCapRefusesNinth(t *testing.T) {
	repo := initWorktreeRepo(t)
	root := t.TempDir()
	mgr := WorktreeManager{}
	for i := uint64(1); i <= DefaultMaxRunWorktrees; i++ {
		if _, err := mgr.Add(context.Background(), WorktreeSpec{
			SourceRepo: repo, WorkspaceDir: root, RunKey: "run", Stage: "implement", Generation: i, TargetRef: "main",
		}); err != nil {
			t.Fatalf("add worktree %d: %v", i, err)
		}
	}
	_, err := mgr.Add(context.Background(), WorktreeSpec{
		SourceRepo: repo, WorkspaceDir: root, RunKey: "run", Stage: "implement", Generation: 9, TargetRef: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "runs.max_worktrees cap") {
		t.Fatalf("ninth worktree error = %v, want cap refusal", err)
	}
}

func TestWorktreeAddValidationAndDefaults(t *testing.T) {
	mgr := WorktreeManager{}
	base := WorktreeSpec{SourceRepo: "/repo", WorkspaceDir: t.TempDir(), RunKey: "run", Stage: "plan", Generation: 1}
	cases := []struct {
		name string
		edit func(*WorktreeSpec)
		want string
	}{
		{"source", func(s *WorktreeSpec) { s.SourceRepo = " " }, "source repo is required"},
		{"workspace", func(s *WorktreeSpec) { s.WorkspaceDir = " " }, "workspace dir is required"},
		{"run key", func(s *WorktreeSpec) { s.RunKey = "" }, "run key, stage, and generation are required"},
		{"stage", func(s *WorktreeSpec) { s.Stage = "" }, "run key, stage, and generation are required"},
		{"generation", func(s *WorktreeSpec) { s.Generation = 0 }, "run key, stage, and generation are required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			tc.edit(&spec)
			if _, err := mgr.Add(context.Background(), spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add error = %v, want %q", err, tc.want)
			}
		})
	}

	var calls []string
	mgr.Runner = func(_ context.Context, dir string, _ string, args ...string) ([]byte, error) {
		calls = append(calls, dir+" "+strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "rev-parse" {
			return []byte("abc123\n"), nil
		}
		return nil, nil
	}
	got, err := mgr.Add(context.Background(), base)
	if err != nil {
		t.Fatalf("Add with default ref/cap: %v", err)
	}
	if got.SHA != "abc123" || !strings.Contains(got.Path, filepath.Join("runs", "run", "plan-1")) {
		t.Fatalf("Add result = %+v", got)
	}
	if len(calls) != 2 || !strings.HasSuffix(calls[0], " worktree add --detach "+got.Path+" HEAD") {
		t.Fatalf("runner calls = %#v, want worktree add with HEAD default", calls)
	}
}

func TestWorktreeAddRunnerErrorsAndCleanup(t *testing.T) {
	wantErr := errors.New("boom")
	mgr := WorktreeManager{Runner: func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" {
			return []byte("nope"), wantErr
		}
		return nil, nil
	}}
	_, err := mgr.Add(context.Background(), WorktreeSpec{
		SourceRepo: "/repo", WorkspaceDir: t.TempDir(), RunKey: "run", Stage: "plan", Generation: 1, TargetRef: "v5",
	})
	if err == nil || !strings.Contains(err.Error(), "git worktree add") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("worktree add error = %v", err)
	}

	root := t.TempDir()
	var removed bool
	mgr.Runner = func(_ context.Context, dir string, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "add":
			if err := os.MkdirAll(args[len(args)-2], 0o770); err != nil {
				t.Fatalf("mkdir fake worktree: %v", err)
			}
			return nil, nil
		case len(args) >= 2 && args[0] == "rev-parse":
			return nil, wantErr
		case len(args) >= 3 && args[0] == "worktree" && args[1] == "remove":
			removed = true
			if dir != "/repo" {
				t.Fatalf("remove dir = %q, want source repo", dir)
			}
			return nil, nil
		default:
			return nil, nil
		}
	}
	_, err = mgr.Add(context.Background(), WorktreeSpec{
		SourceRepo: "/repo", WorkspaceDir: root, RunKey: "run", Stage: "plan", Generation: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "git rev-parse HEAD") || !removed {
		t.Fatalf("rev-parse error = %v removed=%v", err, removed)
	}
}

func TestWorktreeRemoveFallbacksAndHelpers(t *testing.T) {
	mgr := WorktreeManager{}
	if err := mgr.Remove(context.Background(), "/repo", " "); err != nil {
		t.Fatalf("blank Remove: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Remove(context.Background(), "", dir); err != nil {
		t.Fatalf("Remove without source repo: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fallback remove left dir: %v", err)
	}
	dir = filepath.Join(t.TempDir(), "fallback")
	if err := os.MkdirAll(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	mgr.Runner = func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("git remove failed")
	}
	if err := mgr.Remove(context.Background(), "/repo", dir); err != nil {
		t.Fatalf("Remove fallback after git failure: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fallback after git failure left dir: %v", err)
	}
	if got := safePathPart(" !!! "); got != "run" {
		t.Fatalf("safePathPart blank = %q, want run", got)
	}
	if got, err := countLiveRunWorktrees(filepath.Join(t.TempDir(), "missing")); got != 0 || err != nil {
		t.Fatalf("count missing = %d, %v", got, err)
	}
}

func TestWorktreeAddPropagatesFilesystemErrors(t *testing.T) {
	blocked := t.TempDir()
	noAccess := filepath.Join(blocked, "runs")
	if err := os.MkdirAll(noAccess, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(noAccess, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(noAccess, 0o770) })
	if _, err := (WorktreeManager{}).Add(context.Background(), WorktreeSpec{
		SourceRepo: "/repo", WorkspaceDir: blocked, RunKey: "run", Stage: "plan", Generation: 1,
	}); err == nil {
		t.Fatal("Add with unreadable runs root succeeded; want filesystem error")
	}

	fileRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileRoot, "runs"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (WorktreeManager{}).Add(context.Background(), WorktreeSpec{
		SourceRepo: "/repo", WorkspaceDir: fileRoot, RunKey: "run", Stage: "plan", Generation: 1,
	}); err == nil {
		t.Fatal("Add with file at runs root succeeded; want mkdir error")
	}
}

func initWorktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-m", "initial")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}
