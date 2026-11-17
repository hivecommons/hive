package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// startTwoWorkspaceSessions starts two sessions on the same workspace repo and
// returns their worktrees.
func startTwoWorkspaceSessions(t *testing.T, r *UserSessionRuntime) (string, string) {
	t.Helper()
	for _, s := range []crux.UserSessionSpec{workspaceSpec(testSessionID, testSessionSlot, "claude"), workspaceSpec(testSecondSessionID, testSecondSlot, "claude")} {
		if err := r.Start(s); err != nil {
			t.Fatal(err)
		}
	}
	return r.SessionWorkspaceDir(testSessionID, testWorkspaceRepo), r.SessionWorkspaceDir(testSecondSessionID, testWorkspaceRepo)
}

// Deleting a session removes its own worktree (as its uid, through git, with
// the session's branch) and nothing of another session's.
func TestUserSessionRemoveRemovesOnlyItsOwnWorktree(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	ws1, ws2 := startTwoWorkspaceSessions(t, r)
	p.calls = nil
	if err := r.Remove(testSessionID); err != nil {
		t.Fatal(err)
	}
	removes := privCallsWithScript(p, userSessionWorktreeRemoveScript)
	if len(removes) != 1 {
		t.Fatalf("worktree remove ran %d times, want once: %+v", len(removes), removes)
	}
	c := removes[0]
	if c.user != testSessionSpecUser || c.argv[4] != r.WorkspaceDir(testWorkspaceRepo) || c.argv[5] != ws1 || c.argv[6] != userSessionWorktreeBranch(testSessionID) {
		t.Errorf("worktree remove = %s %v, want the session uid removing %s from the clone", c.user, c.argv[4:], ws1)
	}
	for _, gone := range []string{ws1, userSessionWorktreesDir(testSessionID), userSessionSpekInitMarkerDir(testSessionID), userSessionSandboxDir(testSessionID)} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s survived Remove", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(ws2, ".git")); err != nil {
		t.Errorf("the other session's worktree was touched: %v", err)
	}
	if _, err := os.Stat(userSessionSpekInitMarker(testSecondSessionID, "claude")); err != nil {
		t.Errorf("the other session's init marker was touched: %v", err)
	}
}

// A worktree that cannot be removed safely (git failed, so its work was not
// saved) keeps the session: nothing of it is deleted and Remove fails so the
// caller keeps the record and retries.
func TestUserSessionRemoveKeepsEverythingWhenTheWorktreeCannotBeSaved(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	ws1, _ := startTwoWorkspaceSessions(t, r)
	inner := p.handler
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) > 2 && c.argv[2] == userSessionWorktreeRemoveScript {
			return true, []byte("fatal: unable to write new index file"), errors.New("exit 128")
		}
		return inner(c)
	}
	err := r.Remove(testSessionID)
	if err == nil || !strings.Contains(err.Error(), "unable to write new index file") {
		t.Fatalf("Remove = %v, want the worktree failure with git's output", err)
	}
	for _, kept := range []string{filepath.Join(ws1, ".git"), userSessionSandboxDir(testSessionID)} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was deleted although the worktree could not be saved: %v", kept, err)
		}
	}
}

// The sweep removes only worktrees whose session is gone, and leaves anything
// under the worktrees dir that is not a session id alone.
func TestUserSessionSweepWorktreesKeepsLiveSessions(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	ws1, ws2 := startTwoWorkspaceSessions(t, r)
	stray := filepath.Join(userSessionWorkspaceRoot(), userSessionWorktreeDirName, "not-a-session")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	p.calls = nil
	var asked []string
	r.SweepWorktrees(func(id string) bool {
		asked = append(asked, id)
		return id == testSessionID
	})
	if len(asked) != 2 {
		t.Errorf("liveness asked for %v, want both session ids and nothing else", asked)
	}
	removes := privCallsWithScript(p, userSessionWorktreeRemoveScript)
	if len(removes) != 1 || removes[0].argv[5] != ws2 {
		t.Fatalf("sweep removed %+v, want only %s", removes, ws2)
	}
	if _, err := os.Stat(filepath.Join(ws1, ".git")); err != nil {
		t.Errorf("a live session's worktree was swept: %v", err)
	}
	for _, gone := range []string{ws2, userSessionWorktreesDir(testSecondSessionID)} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s survived the sweep", gone)
		}
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("a non-session entry was swept: %v", err)
	}
	// Nothing left to sweep: no git runs at all.
	p.calls = nil
	r.SweepWorktrees(func(id string) bool { return id == testSessionID })
	if n := len(privCallsWithScript(p, userSessionWorktreeRemoveScript)); n != 0 {
		t.Errorf("second sweep ran %d worktree removals", n)
	}
}

// The worktree scripts against a real git (as the test's own uid): a clean
// session leaves nothing behind, uncommitted and detached work is kept on a
// branch of the clone, and a kept branch is checked out again by a later add.
func TestUserSessionWorktreeScriptsOnRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	git := func(in string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir, cmd.Env = in, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	script := func(s string, args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{"-c", s, "sh"}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	clone := filepath.Join(dir, "clone")
	git(dir, "init", "-q", "-b", "main", clone)
	if err := os.WriteFile(filepath.Join(clone, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(clone, "add", "README")
	git(clone, "commit", "-qm", "init")
	add := func(name, branch string) string {
		t.Helper()
		ws := filepath.Join(dir, "wt", name)
		if err := os.MkdirAll(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		script(userSessionWorktreeAddScript, clone, ws, branch)
		return ws
	}
	worktrees := func() int { return len(strings.Split(git(clone, "worktree", "list", "--porcelain"), "\n\n")) }

	// Clean session: worktree and branch go, nothing is reported kept.
	ws := add("clean", "hive-session/aaaa")
	if got := git(ws, "rev-parse", "--abbrev-ref", "HEAD"); got != "hive-session/aaaa" {
		t.Fatalf("worktree is on %q, want its session branch", got)
	}
	if out := script(userSessionWorktreeRemoveScript, clone, ws, "hive-session/aaaa"); strings.TrimSpace(out) != "" {
		t.Errorf("clean removal reported %q", out)
	}
	if _, err := os.Lstat(ws); err == nil || worktrees() != 1 || git(clone, "branch", "--list", "hive-session/*") != "" {
		t.Errorf("clean removal left a worktree or branch behind: %s", git(clone, "worktree", "list"))
	}

	// Dirty session: tracked and untracked changes are committed onto its
	// branch, which is kept.
	ws = add("dirty", "hive-session/bbbb")
	if err := os.WriteFile(filepath.Join(ws, "spec.md"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := script(userSessionWorktreeRemoveScript, clone, ws, "hive-session/bbbb")
	if !strings.Contains(out, "committed uncommitted work") || !strings.Contains(out, "kept branch hive-session/bbbb") {
		t.Errorf("dirty removal reported %q", out)
	}
	if _, err := os.Lstat(ws); err == nil || worktrees() != 1 {
		t.Error("dirty worktree not removed after its work was saved")
	}
	if got := git(clone, "show", "hive-session/bbbb:spec.md"); got != "draft" {
		t.Errorf("untracked file on the kept branch = %q", got)
	}
	if got := git(clone, "show", "hive-session/bbbb:README"); got != "edited" {
		t.Errorf("tracked edit on the kept branch = %q", got)
	}

	// A later worktree for the same branch checks the kept work out again.
	ws = add("again", "hive-session/bbbb")
	if b, err := os.ReadFile(filepath.Join(ws, "spec.md")); err != nil || string(b) != "draft\n" {
		t.Errorf("re-added worktree lacks the kept work: %q, %v", b, err)
	}
	script(userSessionWorktreeRemoveScript, clone, ws, "hive-session/bbbb")

	// Detached HEAD with a commit: saved as <branch>-detached; the session
	// branch itself held nothing and goes.
	ws = add("detached", "hive-session/cccc")
	git(ws, "checkout", "-q", "--detach")
	if err := os.WriteFile(filepath.Join(ws, "detached.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(ws, "add", "detached.md")
	git(ws, "commit", "-qm", "detached work")
	out = script(userSessionWorktreeRemoveScript, clone, ws, "hive-session/cccc")
	if !strings.Contains(out, "kept branch hive-session/cccc-detached") || strings.Contains(out, "kept branch hive-session/cccc\n") {
		t.Errorf("detached removal reported %q", out)
	}
	if got := git(clone, "show", "hive-session/cccc-detached:detached.md"); got != "x" {
		t.Errorf("detached commit not kept: %q", got)
	}
	if git(clone, "branch", "--list", "hive-session/cccc") != "" {
		t.Error("an empty session branch was kept")
	}
}
