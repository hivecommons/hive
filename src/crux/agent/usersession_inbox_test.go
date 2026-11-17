package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/hivecommons/hive/crux"
)

const testInboxFileName = "plaud-rec1.md"

// inboxScriptHandler simulates userSessionInboxScript on the temp tree.
func inboxScriptHandler(c fakePrivCall) (bool, []byte, error) {
	if len(c.argv) < 6 || c.argv[0] != "sh" || c.argv[2] != userSessionInboxScript {
		return false, nil, nil
	}
	d := filepath.Join(c.argv[4], userSessionInboxDir)
	if err := os.MkdirAll(d, 0o770); err != nil {
		return true, nil, err
	}
	return true, nil, os.WriteFile(filepath.Join(d, c.argv[5]), []byte(c.stdin), 0o640)
}

func TestUserSessionStoreInboxFileRunsAsSessionUIDInWorkDir(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.calls = nil
	p.handler = inboxScriptHandler
	path, err := r.StoreInboxFile(testSessionID, "", testInboxFileName, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(userSessionWorkDir(testSessionID), ".hive", "inbox", testInboxFileName)
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if len(p.calls) != 1 || p.calls[0].user != testSessionSpecUser {
		t.Fatalf("the seed file must be written by the session uid:sessions gid only, never root or the hive: %+v", p.calls)
	}
	if strings.Contains(userSessionInboxScript, "chmod") || strings.Contains(userSessionInboxScript, "chgrp") || !strings.Contains(userSessionInboxScript, "umask 027") || !strings.Contains(userSessionInboxScript, "set -C") {
		t.Error("the seed file must get its mode at an exclusive create, never from a path-based chmod/chgrp a peer could redirect")
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
}

func TestUserSessionStoreInboxFileWorkspaceAndValidation(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.handler = inboxScriptHandler
	// The session's own worktree is where its pane runs; the shared clone is
	// still accepted for a record from before per-session worktrees.
	for _, ws := range []string{userSessionWorktreeDir(testSessionID, "hivecommons", "hive"), userSessionWorkspaceDir("hivecommons", "hive")} {
		path, err := r.StoreInboxFile(testSessionID, ws, testInboxFileName, []byte("x"))
		if err != nil || path != filepath.Join(ws, ".hive", "inbox", testInboxFileName) {
			t.Fatalf("workspace seed = %q, %v", path, err)
		}
	}
	ws := userSessionWorkspaceDir("hivecommons", "hive")
	for _, bad := range []string{
		"/etc", filepath.Join(userSessionWorkspaceRoot(), "only-org"), filepath.Join(ws, "sub"), filepath.Join(userSessionWorkspaceRoot(), "..", "x", "y"),
		userSessionWorktreeDir("fedcba987654", "hivecommons", "hive"), // another session's worktree
		filepath.Join(userSessionWorktreesDir(testSessionID), "hivecommons"),
	} {
		if _, err := r.StoreInboxFile(testSessionID, bad, testInboxFileName, []byte("x")); err == nil {
			t.Errorf("workspace %q accepted", bad)
		}
	}
	for _, name := range []string{"../x.md", ".hidden", "a/b.md", "", "-rf", strings.Repeat("a", userSessionUploadNameMax+1)} {
		if _, err := r.StoreInboxFile(testSessionID, "", name, []byte("x")); err == nil {
			t.Errorf("file name %q accepted", name)
		}
	}
	if _, err := r.StoreInboxFile("../etc", "", testInboxFileName, []byte("x")); err == nil {
		t.Error("bad session id accepted")
	}
}

func TestUserSessionStoreInboxFileSymlinkRefused(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.handler = func(c fakePrivCall) (bool, []byte, error) {
		if len(c.argv) >= 3 && c.argv[2] == userSessionInboxScript {
			return true, nil, exitStatusError(userSessionInboxSymlinkExit)
		}
		return false, nil, nil
	}
	if _, err := r.StoreInboxFile(testSessionID, "", testInboxFileName, []byte("x")); !errors.Is(err, crux.ErrUserSessionInboxSymlink) {
		t.Errorf("symlinked inbox dir: %v", err)
	}
}

// TestUserSessionInboxScriptOnRealShell runs the fixed script itself (no
// su-exec: as the test's own uid and group) and asserts what it leaves
// behind: mode 0640, the process group, a .gitignore, a replaced re-send, and a
// refusal at a symlinked .hive.
func TestUserSessionInboxScriptOnRealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ws := t.TempDir()
	gid := strconv.Itoa(os.Getgid())
	run := func(dir, content string) error {
		cmd := exec.Command("sh", "-c", userSessionInboxScript, "sh", dir, testInboxFileName)
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(err.Error() + ": " + string(out))
		}
		return nil
	}
	if err := run(ws, "first"); err != nil {
		t.Fatal(err)
	}
	if err := run(ws, "second"); err != nil {
		t.Fatalf("re-send must replace the file: %v", err)
	}
	path := filepath.Join(ws, ".hive", "inbox", testInboxFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && strconv.Itoa(int(st.Gid)) != gid {
		t.Errorf("group = %d, want %s", st.Gid, gid)
	}
	if b, _ := os.ReadFile(path); string(b) != "second" {
		t.Errorf("content = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".hive", "inbox", ".gitignore")); string(b) != "*\n" {
		t.Errorf(".gitignore = %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Join(ws, ".hive", "inbox")); len(ents) != 2 {
		t.Errorf("left temp files behind: %v", ents)
	}

	evil := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(evil, ".hive")); err != nil {
		t.Fatal(err)
	}
	if err := run(evil, "x"); err == nil || !strings.Contains(err.Error(), "exit status "+userSessionInboxSymlinkExitText) {
		t.Errorf("symlinked .hive must be refused with %d: %v", userSessionInboxSymlinkExit, err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("wrote through the symlink: %v", ents)
	}

	// A directory planted at the target name is refused, not written into.
	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, ".hive", "inbox", testInboxFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(planted, "x"); err == nil || !strings.Contains(err.Error(), "exit status "+userSessionInboxSymlinkExitText) {
		t.Errorf("a directory at the target must be refused: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(planted, ".hive", "inbox", testInboxFileName)); len(ents) != 0 {
		t.Errorf("wrote into the planted directory: %v", ents)
	}
}
