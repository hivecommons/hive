package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Regressions behind the first session on a hosted hive (clone of the
// project repository, hand-over to the sessions group, attach through ttyd):
//
//   - the root hand-over ran under the 10-second tmux command timeout, Go then
//     tried to kill a root child the hive's uid may not signal ("exec: killing
//     Cmd: operation not permitted") and the start failed although the
//     workspace had in fact been moved into place;
//   - the session's tmux socket dir stayed 0700 uid:<sessions gid>, so the
//     terminal attacher (the hive's uid) could not stat the socket and every
//     attach died at ttyd's wait.

// The invariant: the hand-over (and the removal of a stale staging tree) runs
// under its own long deadline, never under the 10-second command timeout and
// never under a context a caller could cancel.
func TestUserSessionWorkspaceHandoffHasItsOwnDeadline(t *testing.T) {
	r, _, p, _ := workspaceRuntime(t)
	ws := r.WorkspaceDir(testWorkspaceRepo)
	staging := ws + userSessionWorkspaceCloningSuffix
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	type privCtx struct {
		argv     []string
		deadline time.Time
		hasDl    bool
		err      error
	}
	var seen []privCtx
	inner := r.priv
	r.priv = func(ctx context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error) {
		dl, ok := ctx.Deadline()
		seen = append(seen, privCtx{argv: argv, deadline: dl, hasDl: ok, err: ctx.Err()})
		return inner(ctx, user, stdin, argv...)
	}
	before := time.Now()
	if err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")); err != nil {
		t.Fatal(err)
	}
	var handoff, staleRm *privCtx
	for i := range seen {
		c := &seen[i]
		switch {
		case len(c.argv) >= 3 && c.argv[0] == "sh" && c.argv[2] == userSessionWorkspaceOwnScript:
			handoff = c
		case c.argv[0] == "rm" && c.argv[len(c.argv)-1] == staging:
			staleRm = c
		}
	}
	if handoff == nil || staleRm == nil {
		t.Fatalf("hand-over or stale-staging removal did not run: %+v", seen)
	}
	for name, c := range map[string]*privCtx{"hand-over": handoff, "stale staging removal": staleRm} {
		if c.err != nil {
			t.Errorf("%s ran under an already cancelled context: %v", name, c.err)
		}
		if !c.hasDl {
			t.Errorf("%s has no deadline at all", name)
			continue
		}
		if remaining := c.deadline.Sub(before); remaining <= userSessionCmdTimeout || remaining < userSessionWorkspaceHandoffTimeout/2 {
			t.Errorf("%s deadline is %s away, want the hand-over timeout (%s), not the command timeout (%s)", name, remaining.Round(time.Second), userSessionWorkspaceHandoffTimeout, userSessionCmdTimeout)
		}
	}
	if len(p.calls) == 0 {
		t.Fatal("no privileged calls recorded")
	}
	if _, err := os.Lstat(ws); err != nil {
		t.Errorf("workspace not in place after start: %v", err)
	}
}

// The invariant: a hand-over that fails leaves neither a workspace nor a
// staging tree behind, so the next start clones afresh instead of tripping
// over a half-owned tree, and the start reports the failure.
func TestUserSessionWorkspaceFailedHandoffLeavesNothingBehind(t *testing.T) {
	r, f, p, _ := workspaceRuntime(t)
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) >= 3 && c.argv[0] == "sh" && c.argv[2] == userSessionWorkspaceOwnScript {
			return true, []byte("chown: changing ownership: Operation not permitted"), errors.New("exit status 1")
		}
		return false, nil, nil
	}
	err := r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude"))
	if err == nil || !strings.Contains(err.Error(), "handing workspace to the sessions group") {
		t.Fatalf("Start error = %v, want the hand-over failure", err)
	}
	ws := r.WorkspaceDir(testWorkspaceRepo)
	for _, path := range []string{ws, ws + userSessionWorkspaceCloningSuffix} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s left behind after a failed hand-over", path)
		}
	}
	if indexOf(f.verbs(), "respawn-pane") >= 0 {
		t.Error("CLI launched although the workspace was never handed over")
	}
}

// The invariant: two sessions started at the same time share ONE clone. The
// runtime's workspace mutex makes the second wait for the first's clone and
// then find the workspace in place; it never clones again and never fails.
func TestUserSessionWorkspaceConcurrentStartsCloneOnce(t *testing.T) {
	r, f, p, g := workspaceRuntime(t)
	// The fakes are not safe for concurrent use; every fake call takes one
	// lock, released again before the clone blocks, so both starts can
	// interleave freely.
	var mu sync.Mutex
	run, priv := r.run, r.priv
	r.run = func(ctx context.Context, user string, env []string, stdin, socket string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return run(ctx, user, env, stdin, socket, args...)
	}
	r.priv = func(ctx context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return priv(ctx, user, stdin, argv...)
	}
	cloneEntered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.git = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		once.Do(func() { close(cloneEntered) })
		<-release
		mu.Lock()
		defer mu.Unlock()
		return g.run(ctx, dir, env, args...)
	}
	errs := make(chan error, 2)
	go func() { errs <- r.Start(workspaceSpec(testSessionID, testSessionSlot, "claude")) }()
	<-cloneEntered
	go func() { errs <- r.Start(workspaceSpec(testSecondSessionID, testSecondSlot, "codex")) }()
	// Whether the second start is already waiting on the workspace mutex or
	// still preparing its sandbox when the clone is released, the invariant is
	// the same: it finds the workspace and never clones.
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Start = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent start never returned")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(g.calls) != 1 {
		t.Fatalf("git ran %d times for two concurrent starts, want one clone: %v", len(g.calls), g.calls)
	}
	if n := len(privCallsWithScript(p, userSessionWorkspaceOwnScript)); n != 1 {
		t.Errorf("hand-over ran %d times, want once", n)
	}
	// Each opened in its own worktree of that one clone (#18588), added once
	// per session.
	clone := r.WorkspaceDir(testWorkspaceRepo)
	cwds := map[string]bool{}
	for _, c := range f.calls {
		if c.args[0] == "respawn-pane" {
			id := strings.TrimPrefix(c.socket, UserSessionTmuxPrefix)
			cwd := c.args[indexOf(c.args, "-c")+1]
			if cwd != r.SessionWorkspaceDir(id, testWorkspaceRepo) {
				t.Errorf("session %s opened in %s, want its own worktree", id, cwd)
			}
			cwds[cwd] = true
		}
	}
	if len(cwds) != 2 {
		t.Errorf("sessions opened in %v, want two distinct worktrees", cwds)
	}
	adds := privCallsWithScript(p, userSessionWorktreeAddScript)
	if len(adds) != 2 || adds[0].argv[4] != clone || adds[1].argv[4] != clone || adds[0].argv[5] == adds[1].argv[5] {
		t.Errorf("worktree adds = %+v, want one per session against the one clone %s", adds, clone)
	}
}

// The invariant: a root child is cancelled through the helper, in its own
// process group, and Go is never left to send the raw kill the hive's uid may
// not send; an unprivileged child keeps the plain context kill and WaitDelay.
func TestNewUserSessionPrivCmdRootChildIsCancelledThroughHelper(t *testing.T) {
	root, err := newUserSessionPrivCmd(context.Background(), userSessionRootSpec, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatal(err)
	}
	if root.SysProcAttr == nil || !root.SysProcAttr.Setpgid {
		t.Error("root child is not in its own process group; the helper could not kill what it spawned")
	}
	if root.Cancel == nil {
		t.Error("root child has no helper-based Cancel; Go would Process.Kill a root pid")
	}
	if root.WaitDelay != 0 {
		t.Errorf("root child WaitDelay = %s; after it Go falls back to a raw Kill the hive may not send", root.WaitDelay)
	}
	if root.Args[0] != userSuExecBinary || root.Args[1] != userSessionRootSpec {
		t.Errorf("argv = %v", root.Args)
	}
	for _, kv := range root.Env {
		if !strings.HasPrefix(kv, "PATH=") {
			t.Errorf("root child inherits %q from the hive environment", kv)
		}
	}
	sess, err := newUserSessionPrivCmd(context.Background(), testSessionSpecUser, nil, "true")
	if err != nil {
		t.Fatal(err)
	}
	// (exec.CommandContext always installs a default Cancel, the plain
	// Process.Kill, so only the process group and WaitDelay distinguish it.)
	if sess.SysProcAttr != nil || sess.WaitDelay != tmuxCmdWaitDelay {
		t.Errorf("session-uid child changed shape: pgid=%v waitDelay=%s", sess.SysProcAttr, sess.WaitDelay)
	}
	if _, err := newUserSessionPrivCmd(context.Background(), "", nil, "true"); err == nil {
		t.Error("empty identity accepted")
	}
	if _, err := newUserSessionPrivCmd(context.Background(), userSessionRootSpec, nil); err == nil {
		t.Error("empty command accepted")
	}
}

// The invariant: right after the session's tmux server is created, root makes
// its socket dir /tmp/tmux-<uid> traversable by the terminal attacher's group
// (0710, the hive's gid, exactly as agent socket dirs are) while refusing a
// dir the session uid does not own; the socket file itself is untouched.
func TestUserSessionSocketDirOpenedToAttacher(t *testing.T) {
	_, f, p := startedRuntime(t)
	calls := privCallsWithScript(p, userSessionSocketDirScript)
	if len(calls) != 1 {
		t.Fatalf("socket dir script ran %d times, want once per start: %+v", len(calls), p.calls)
	}
	c := calls[0]
	if c.user != userSessionRootSpec {
		t.Errorf("socket dir opened as %q, want root (the session uid cannot chgrp to a group it is not in)", c.user)
	}
	wantDir := userSessionTmuxSocketDir(testSessionUID)
	if len(c.argv) != 7 || c.argv[4] != wantDir || c.argv[5] != strconv.Itoa(os.Getgid()) || c.argv[6] != strconv.Itoa(testSessionUID) {
		t.Errorf("socket dir args = %v, want [sh -c <script> sh %s %d %d]", c.argv, wantDir, os.Getgid(), testSessionUID)
	}
	// The server was created (tmux makes the dir) and the CLI launched after.
	created, launched := false, false
	for _, tc := range f.calls {
		if indexOf(tc.args, "new-session") >= 0 {
			created = true
		}
		if tc.args[0] == "respawn-pane" {
			launched = true
		}
	}
	if !created || !launched {
		t.Fatalf("missing step: new-session=%v respawn=%v", created, launched)
	}
	for _, want := range []string{"chmod " + userSessionSocketDirMode, `chgrp -- "$g" "$d"`, `[ ! -L "$d" ]`, `stat -c %u -- "$d")" = "$u"`} {
		if !strings.Contains(userSessionSocketDirScript, want) {
			t.Errorf("socket dir script lacks %q", want)
		}
	}
	for _, bad := range []string{"777", "o+", "a+", "chown"} {
		if strings.Contains(userSessionSocketDirScript, bad) {
			t.Errorf("socket dir script contains %q: it must only open the dir to the attacher's group, keep the owner and never open it to others", bad)
		}
	}
	t.Setenv(userSessionAttachGIDEnv, "4242")
	if got := userSessionAttachGID(); got != 4242 {
		t.Errorf("attach gid override = %d", got)
	}
	t.Setenv(userSessionAttachGIDEnv, "")
	if got := userSessionAttachGID(); got != os.Getgid() {
		t.Errorf("attach gid default = %d, want the hive's gid %d", got, os.Getgid())
	}
}

// A session whose socket dir cannot be opened to the attacher is not started:
// nobody could ever attach it.
func TestUserSessionSocketDirFailureFailsStart(t *testing.T) {
	f := sessionLifecycleTmux("")
	r, p := newTestSessionRuntimeWithPriv(t, f, sessionSecretEnv)
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) >= 3 && c.argv[0] == "sh" && c.argv[2] == userSessionSocketDirScript {
			return true, []byte("chgrp: Operation not permitted"), errors.New("exit status 1")
		}
		return false, nil, nil
	}
	err := r.Start(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", UIDSlot: testSessionSlot})
	if err == nil || !strings.Contains(err.Error(), "opening the session socket dir") {
		t.Fatalf("Start = %v, want the socket dir failure", err)
	}
	if indexOf(f.verbs(), "respawn-pane") >= 0 {
		t.Error("CLI launched in a session nobody can attach")
	}
}
