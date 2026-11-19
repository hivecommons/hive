package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// realGitWorkspace is a shared clone, made the way the hive makes one (a
// clone with core.sharedRepository=group) and then locked by
// userSessionWorkspaceSecureScript, all as the test's own uid. Root's chown is
// replaced by a no-op on PATH, since the test cannot change owners: everything
// else the lock script does (rewriting the config, emptying hooks, the mode
// bits) runs for real. It returns the clone, a git helper, a script runner and
// the environment both use.
type realGitWorkspace struct {
	dir, clone string
	env        []string
	t          *testing.T
}

func newRealGitWorkspace(t *testing.T) *realGitWorkspace {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the workspace scripts target the Linux image (GNU coreutils)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shim, "chown"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &realGitWorkspace{dir: dir, clone: filepath.Join(dir, "clone"), t: t,
		env: append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "PATH="+shim+":"+os.Getenv("PATH"))}
	origin := filepath.Join(dir, "origin")
	w.git(dir, "init", "-q", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git(origin, "add", "README")
	w.git(origin, "commit", "-qm", "init")
	w.git(dir, append(append([]string{}, userSessionHiveGitArgs...), "clone", "-q", "-c", "core.sharedRepository=group", "--", origin, w.clone)...)
	// What a session could plant while the config and hooks were
	// group-writable (before this release): all of it must go.
	gd := filepath.Join(w.clone, ".git")
	w.git(w.clone, "config", "core.fsmonitor", "touch "+filepath.Join(dir, "pwned-fsmonitor"))
	w.git(w.clone, "config", "alias.st", "!touch "+filepath.Join(dir, "pwned-alias"))
	if err := os.WriteFile(filepath.Join(gd, "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "pwned-hook")+"\n"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gd, "info", "attributes"), []byte("* filter=evil\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	w.script(userSessionWorkspaceSecureScript, w.clone, strconv.Itoa(os.Getgid()), "https://github.com/acme/widgets.git", userSessionWorktreeBranchDir)
	return w
}

func (w *realGitWorkspace) git(in string, args ...string) string {
	w.t.Helper()
	out, err := w.gitErr(in, args...)
	if err != nil {
		w.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func (w *realGitWorkspace) gitErr(in string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir, cmd.Env = in, w.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (w *realGitWorkspace) scriptErr(s string, args ...string) (string, error) {
	cmd := exec.Command("sh", append([]string{"-c", s, "sh"}, args...)...)
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (w *realGitWorkspace) script(s string, args ...string) string {
	w.t.Helper()
	out, err := w.scriptErr(s, args...)
	if err != nil {
		w.t.Fatalf("script %v: %v: %s", args, err, out)
	}
	return out
}

// add makes the session's worktree dir and runs the add script, as the hive
// would (minus root's hand-over, which only changes the owner).
func (w *realGitWorkspace) add(name, branch string) (string, string) {
	w.t.Helper()
	ws := filepath.Join(w.dir, "wt", name)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		w.t.Fatal(err)
	}
	return ws, w.script(userSessionWorktreeAddScript, w.clone, ws, branch)
}

// remove runs the remove script and then deletes the worktree dir, as root's
// rm -rf in removeWorktrees does.
func (w *realGitWorkspace) remove(ws, branch string) string {
	w.t.Helper()
	out := w.script(userSessionWorktreeRemoveScript, w.clone, ws, branch)
	if err := os.RemoveAll(ws); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// The lock (the owner's chosen design for #18588): the shared clone's config,
// hooks, info and modules can be written by root alone, while objects, refs
// and logs stay group-writable for commits and fetches. Whatever a session
// planted before is gone, and a session's own settings live in its worktree's
// config.worktree.
func TestUserSessionWorkspaceLockOnRealGit(t *testing.T) {
	w := newRealGitWorkspace(t)
	gd := filepath.Join(w.clone, ".git")
	const groupWrite = 0o020
	for _, f := range []string{"config", "config.worktree", "commondir", "HEAD", "packed-refs"} {
		p := filepath.Join(gd, f)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) && f == "packed-refs" {
			continue
		}
		if m := modeOf(t, p); !m.IsRegular() || m.Perm() != 0o644 {
			t.Errorf(".git/%s is %v, want a regular file 0644 (not group-writable)", f, m)
		}
	}
	for _, d := range []string{"hooks", "info", "modules"} {
		m := modeOf(t, filepath.Join(gd, d))
		if !m.IsDir() || m.Perm()&groupWrite != 0 || m.Perm() != 0o755 {
			t.Errorf(".git/%s is %v, want a 0755 directory", d, m)
		}
		if entries, err := os.ReadDir(filepath.Join(gd, d)); err != nil || len(entries) != 0 {
			t.Errorf(".git/%s still holds %v (%v)", d, entries, err)
		}
	}
	for _, d := range []string{"objects", "refs", "logs", "refs/heads/" + userSessionWorktreeBranchDir, "logs/refs/heads/" + userSessionWorktreeBranchDir} {
		if m := modeOf(t, filepath.Join(gd, d)); !m.IsDir() || m.Perm() != 0o770 || m&os.ModeSetgid == 0 {
			t.Errorf(".git/%s is %v, want a setgid group-writable directory", d, m)
		}
	}
	for _, d := range []string{gd, filepath.Join(gd, "worktrees"), w.clone} {
		if m := modeOf(t, d); m.Perm() != 0o770 || m&os.ModeSticky == 0 || m&os.ModeSetgid == 0 {
			t.Errorf("%s is %v, want 3770: group-writable, but only an entry's owner may rename or delete it", d, m)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(gd, "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"fsmonitor", "alias", "pwned"} {
		if strings.Contains(string(cfg), bad) {
			t.Errorf("the rewritten config kept a planted %q:\n%s", bad, cfg)
		}
	}
	if got := w.git(w.clone, "-c", "safe.directory=*", "config", "extensions.worktreeConfig"); got != "true" {
		t.Errorf("extensions.worktreeConfig = %q", got)
	}
	if got := w.git(w.clone, "-c", "safe.directory=*", "config", "remote.origin.url"); got != "https://github.com/acme/widgets.git" {
		t.Errorf("origin = %q, want the plain https URL", got)
	}
	if got := w.git(w.clone, "-c", "safe.directory=*", "config", "branch.main.merge"); got != "refs/heads/main" {
		t.Errorf("branch.main.merge = %q", got)
	}
	wantCommon, err := filepath.EvalSymlinks(gd)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := filepath.EvalSymlinks(w.git(w.clone, "-c", "safe.directory=*", "rev-parse", "--path-format=absolute", "--git-common-dir")); err != nil || got != wantCommon {
		t.Errorf("the placeholder commondir moved the common dir to %q (%v), want %q", got, err, wantCommon)
	}
	// Locking twice (every boot) is a no-op.
	w.script(userSessionWorkspaceSecureScript, w.clone, strconv.Itoa(os.Getgid()), "https://github.com/acme/widgets.git", userSessionWorktreeBranchDir)

	// git works under the lock: add, commit, branch, worktree config.
	ws, out := w.add("work", "hive-session/aaaa")
	if !strings.Contains(out, userSessionWorktreeAddedLine) {
		t.Fatalf("add reported %q", out)
	}
	if err := os.WriteFile(filepath.Join(ws, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git(ws, "add", "f")
	w.git(ws, "commit", "-qm", "work")
	w.git(ws, "branch", "scratch")
	w.git(ws, "branch", "-D", "scratch")
	w.git(ws, "config", "--worktree", "user.name", "Session Person")
	if got := w.git(ws, "config", "--worktree", "--get", "user.name"); got != "Session Person" {
		t.Errorf("worktree config user.name = %q", got)
	}
	entry := w.git(ws, "rev-parse", "--absolute-git-dir")
	for _, f := range []string{"config.worktree", "commondir", "gitdir"} {
		if m := modeOf(t, filepath.Join(entry, f)); m.Perm()&groupWrite != 0 {
			t.Errorf("worktree entry %s is %v: another session could redirect or configure this one", f, m)
		}
	}
	if m := modeOf(t, entry); m.Perm()&groupWrite != 0 {
		t.Errorf("worktree entry %s is %v: another session could plant files in it", entry, m)
	}
	if strings.TrimSpace(w.git(ws, "log", "-1", "--format=%s")) != "work" {
		t.Error("commit in the worktree did not land")
	}
	for _, f := range []string{"pwned-fsmonitor", "pwned-alias", "pwned-hook"} {
		if _, err := os.Stat(filepath.Join(w.dir, f)); err == nil {
			t.Errorf("a planted command ran (%s)", f)
		}
	}
}

// The worktree scripts against a real git under the lock (as the test's own
// uid): a resume is a no-op that keeps the work, a clean session leaves
// nothing behind, uncommitted and detached work is auto-saved on a branch of
// the clone, a kept branch is checked out again by a later add, a missing
// clone is reported, and a worktree whose .git points anywhere else is
// refused without touching anything.
func TestUserSessionWorktreeScriptsOnRealGit(t *testing.T) {
	w := newRealGitWorkspace(t)
	worktrees := func() int {
		return len(strings.Split(w.git(w.clone, "-c", "safe.directory=*", "worktree", "list", "--porcelain"), "\n\n"))
	}
	inClone := func(args ...string) string {
		return w.git(w.clone, append([]string{"-c", "safe.directory=*"}, args...)...)
	}

	// Resume: re-adding an existing worktree (which the hive's own uid
	// cannot see inside) is a no-op that keeps the work and reports no add.
	ws, _ := w.add("resume", "hive-session/rrrr")
	if err := os.WriteFile(filepath.Join(ws, "wip.md"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out := w.add("resume", "hive-session/rrrr"); strings.Contains(out, userSessionWorktreeAddedLine) {
		t.Errorf("re-adding an existing worktree reported a fresh add: %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "wip.md")); err != nil || string(b) != "wip\n" {
		t.Errorf("re-adding an existing worktree touched its work: %q, %v", b, err)
	}
	w.remove(ws, "hive-session/rrrr")
	inClone("branch", "-D", "hive-session/rrrr")

	// A missing clone: both scripts say so and touch nothing.
	orphan := filepath.Join(w.dir, "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{userSessionWorktreeRemoveScript, userSessionWorktreeAddScript} {
		if out := w.script(s, filepath.Join(w.dir, "no-clone"), orphan, "hive-session/oooo"); !strings.Contains(out, userSessionWorktreeCloneGoneLine) {
			t.Errorf("script against a missing clone reported %q", out)
		}
	}
	if entries, err := os.ReadDir(orphan); err != nil || len(entries) != 0 {
		t.Errorf("a script against a missing clone changed the worktree dir: %v, %v", entries, err)
	}

	// Clean session: worktree and branch go, nothing is reported kept.
	ws, _ = w.add("clean", "hive-session/aaaa")
	if got := w.git(ws, "rev-parse", "--abbrev-ref", "HEAD"); got != "hive-session/aaaa" {
		t.Fatalf("worktree is on %q, want its session branch", got)
	}
	if out := w.remove(ws, "hive-session/aaaa"); strings.TrimSpace(out) != "" {
		t.Errorf("clean removal reported %q", out)
	}
	if worktrees() != 1 || inClone("branch", "--list", "hive-session/*") != "" {
		t.Errorf("clean removal left a worktree or branch behind: %s", inClone("worktree", "list"))
	}

	// Dirty session: tracked and untracked changes are auto-saved onto its
	// branch, which is kept.
	ws, _ = w.add("dirty", "hive-session/bbbb")
	if err := os.WriteFile(filepath.Join(ws, "spec.md"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := w.remove(ws, "hive-session/bbbb")
	if !strings.Contains(out, "auto-saved uncommitted work") || !strings.Contains(out, "kept branch hive-session/bbbb") {
		t.Errorf("dirty removal reported %q", out)
	}
	if worktrees() != 1 {
		t.Error("dirty worktree's entry not removed after its work was saved")
	}
	if got := inClone("show", "hive-session/bbbb:spec.md"); got != "draft" {
		t.Errorf("untracked file on the kept branch = %q", got)
	}
	if got := inClone("show", "hive-session/bbbb:README"); got != "edited" {
		t.Errorf("tracked edit on the kept branch = %q", got)
	}
	if got := inClone("log", "-1", "--format=%s", "hive-session/bbbb"); got != userSessionWorktreeAutoSaveMessage {
		t.Errorf("auto-save commit message = %q", got)
	}

	// A later worktree for the same branch checks the kept work out again.
	ws, _ = w.add("again", "hive-session/bbbb")
	if b, err := os.ReadFile(filepath.Join(ws, "spec.md")); err != nil || string(b) != "draft\n" {
		t.Errorf("re-added worktree lacks the kept work: %q, %v", b, err)
	}
	w.remove(ws, "hive-session/bbbb")

	// Detached HEAD with a commit: saved as <branch>-detached; the session
	// branch itself held nothing and goes.
	ws, _ = w.add("detached", "hive-session/cccc")
	w.git(ws, "checkout", "-q", "--detach")
	if err := os.WriteFile(filepath.Join(ws, "detached.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.git(ws, "add", "detached.md")
	w.git(ws, "commit", "-qm", "detached work")
	out = w.remove(ws, "hive-session/cccc")
	if !strings.Contains(out, "kept branch hive-session/cccc-detached") || strings.Contains(out, "kept branch hive-session/cccc\n") {
		t.Errorf("detached removal reported %q", out)
	}
	if got := inClone("show", "hive-session/cccc-detached:detached.md"); got != "x" {
		t.Errorf("detached commit not kept: %q", got)
	}
	if inClone("branch", "--list", "hive-session/cccc") != "" {
		t.Error("an empty session branch was kept")
	}

	// A worktree whose .git was swapped for one pointing elsewhere (the
	// worktree dir is group-writable): neither script runs git there, and
	// the removal fails without deleting anything.
	ws, _ = w.add("swapped", "hive-session/dddd")
	evil := filepath.Join(w.dir, "evil")
	w.git(w.dir, "init", "-q", "--bare", evil)
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := w.scriptErr(userSessionWorktreeAddScript, w.clone, ws, "hive-session/dddd"); err == nil {
		t.Errorf("resume into a swapped .git succeeded: %q", out)
	}
	if out, err := w.scriptErr(userSessionWorktreeRemoveScript, w.clone, ws, "hive-session/dddd"); err == nil || !strings.Contains(out, "not this session's worktree") {
		t.Errorf("removal of a swapped .git = %q, %v; want a refusal", out, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "README")); err != nil {
		t.Errorf("a refused removal deleted the worktree's files: %v", err)
	}
}
