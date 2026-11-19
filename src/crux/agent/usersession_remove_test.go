package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// "Nothing to adopt" is success: an ended session whose HOME holds an expired
// login, no login file at all, or a copy without a refresh token is deleted,
// and the shared login is not touched. Synthetic credentials only.
func TestSessionRemove_NothingToAdoptIsSuccess(t *testing.T) {
	now := time.Now()
	cases := map[string]func(t *testing.T, path string){
		"expired login copy": func(t *testing.T, path string) {
			writeLoginAt(t, path, bridgeTestCreds(t, "old", now.Add(-time.Hour).UnixMilli()), now)
		},
		"no login file": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
		"copy without a refresh token": func(t *testing.T, path string) {
			// What Start installed: asserted, then left as it is.
			assertRefreshless(t, path, "acc-shared-"+bridgeTestTokenSentinel)
		},
		"not a credentials file": func(t *testing.T, path string) {
			writeLoginAt(t, path, []byte("not json"), now)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSessionLoginFixture(t)
			shared := bridgeTestCreds(t, "shared", now.Add(time.Hour).UnixMilli())
			writeLoginAt(t, f.target, shared, now.Add(-time.Hour))
			f.start(t)
			prepare(t, sessionLoginPath(testSessionID))

			if err := f.r.Remove(testSessionID); err != nil {
				t.Fatalf("Remove = %v, want success: there is nothing to adopt", err)
			}
			if _, err := os.Lstat(userSessionSandboxDir(testSessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the sandbox survived Remove (%v)", err)
			}
			if !f.r.Removed(testSessionID) {
				t.Error("Removed = false after a successful Remove")
			}
			if !bytes.Equal(mustRead(t, f.target), shared) || f.adoptions() != 0 {
				t.Error("deleting a session with nothing to adopt changed the shared login")
			}
			f.assertNoTokenMaterial(t)
		})
	}
}

// A real adoption failure of a strictly newer, usable login refuses the
// delete with the reason, before anything is removed.
func TestSessionRemove_AdoptionFailureRemovesNothing(t *testing.T) {
	f := newSessionLoginFixture(t)
	now := time.Now()
	f.start(t)
	path := sessionLoginPath(testSessionID)
	rotated := bridgeTestCreds(t, "rotated", now.Add(8*time.Hour).UnixMilli())
	writeLoginAt(t, path, rotated, now)
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), f.target); err != nil {
		t.Fatal(err)
	}
	f.p.calls = nil

	err := f.r.Remove(testSessionID)
	if !errors.Is(err, crux.ErrUserSessionHoldsLogin) {
		t.Fatalf("Remove = %v, want ErrUserSessionHoldsLogin", err)
	}
	if !strings.Contains(err.Error(), "this session holds the hive's current sign-in; adopting it failed: ") {
		t.Errorf("reason = %q", err)
	}
	if strings.Contains(err.Error(), bridgeTestTokenSentinel) {
		t.Error("token material in the error")
	}
	if !bytes.Equal(mustRead(t, path), rotated) {
		t.Error("the session's login was lost")
	}
	for _, c := range f.p.calls {
		if len(c.argv) > 0 && c.argv[0] == "rm" {
			t.Errorf("a refused delete removed something: %v", c.argv)
		}
		if len(c.argv) > 2 && c.argv[2] == userSessionWorktreeRemoveScript {
			t.Error("a refused delete removed the worktree")
		}
	}
	if f.r.Removed(testSessionID) {
		t.Error("Removed = true for a session whose delete was refused")
	}
	f.assertNoTokenMaterial(t)
}

// The incident (#18767): the removal command reports an error (the cancel of
// its timeout failed) although it deleted the sandbox. What decides is the
// directory.
func TestSessionRemove_CommandErrorWithDirectoryGoneIsSuccess(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) > 0 && c.argv[0] == "rm" {
			_ = os.RemoveAll(c.argv[len(c.argv)-1])
			return true, []byte("su-exec: kill: No such file or directory"), errors.New("exec: canceling Cmd: killing privileged child: exit status 1")
		}
		return false, nil, nil
	}
	if err := r.Remove(testSessionID); err != nil {
		t.Fatalf("Remove = %v, want success: the directory is gone", err)
	}
	if !r.Removed(testSessionID) {
		t.Error("Removed = false")
	}
}

func TestSessionRemove_CommandErrorWithDirectoryLeftIsFailure(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) > 0 && c.argv[0] == "rm" {
			return true, []byte("Device or resource busy"), errors.New("exit status 1")
		}
		return false, nil, nil
	}
	err := r.Remove(testSessionID)
	if err == nil || !strings.Contains(err.Error(), "removing session sandbox") {
		t.Fatalf("Remove = %v, want the sandbox failure", err)
	}
	if r.Removed(testSessionID) {
		t.Error("Removed = true although the sandbox is still there")
	}
	// A second call, with the cause gone, completes.
	p.handler = nil
	if err := r.Remove(testSessionID); err != nil {
		t.Fatalf("second Remove = %v", err)
	}
	if !r.Removed(testSessionID) {
		t.Error("Removed = false after the retry")
	}
}

// Deleting a sandbox is not a tmux call: it gets its own deadline, far past
// the 10 seconds that used to cancel it half-way.
func TestSessionRemove_SandboxRemovalHasItsOwnDeadline(t *testing.T) {
	r, _, p := startedRuntime(t)
	var left time.Duration
	r.priv = func(ctx context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error) {
		if len(argv) > 0 && argv[0] == "rm" && argv[len(argv)-1] == userSessionSandboxDir(testSessionID) {
			if dl, ok := ctx.Deadline(); ok {
				left = time.Until(dl)
			}
		}
		return p.run(ctx, user, stdin, argv...)
	}
	if err := r.Remove(testSessionID); err != nil {
		t.Fatal(err)
	}
	if left <= userSessionCmdTimeout*6 {
		t.Errorf("the sandbox removal had %v, want far more than the tmux command timeout %v", left, userSessionCmdTimeout)
	}
}

func TestUserSessionRemoved(t *testing.T) {
	r, _, _ := startedRuntime(t)
	if r.Removed(testSessionID) {
		t.Error("Removed = true for a session whose sandbox exists")
	}
	if r.Removed("../x") {
		t.Error("Removed = true for a bad id")
	}
	const other = "ba9876543210"
	if !r.Removed(other) {
		t.Error("Removed = false for an id with nothing on disk")
	}
	if err := os.MkdirAll(userSessionWorktreesDir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if r.Removed(other) {
		t.Error("Removed = true while a worktrees dir is left")
	}
	// A sandbox root that cannot be seen proves nothing.
	t.Setenv(userSessionSandboxDirEnv, filepath.Join(t.TempDir(), "not-mounted"))
	if r.Removed("aaaaaaaaaaaa") {
		t.Error("Removed = true although the sandbox root is missing")
	}
}

// The cancel of a root child goes through the shell's builtin kill: the image
// has no kill binary, so `su-exec root kill` could never run.
func TestUserSessionRootKillUsesTheShellBuiltin(t *testing.T) {
	const pid = 4242
	want := []string{userSessionRootSpec, "sh", "-c", userSessionRootKillScript, "sh", strconv.Itoa(pid)}
	if got := userSessionRootKillArgv(pid); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if got := userSessionRootKillArgv(pid); got[1] == "kill" {
		t.Error("the cancel execs a kill binary")
	}

	// The script really ends a process group, with PATH emptied so that only
	// a builtin can do it.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	child := exec.Command(sh, "-c", "sleep 300 & wait")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	kill := exec.Command(sh, "-c", userSessionRootKillScript, "sh", strconv.Itoa(child.Process.Pid))
	kill.Env = []string{"PATH=/nonexistent"}
	if out, err := kill.CombinedOutput(); err != nil {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		t.Fatalf("kill script: %v: %s", err, out)
	}
	select {
	case werr := <-done:
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			t.Fatalf("child ended with %v, want a signal", werr)
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Errorf("child ended with %v, want SIGKILL", werr)
		}
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		t.Fatal("the process group survived the kill script")
	}
}
