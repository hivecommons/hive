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
	if got := p.calls[0].argv[6]; got != strconv.Itoa(defaultUserSessionGID) {
		t.Errorf("chgrp target = %q, want the sessions gid", got)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Errorf("content = %q", b)
	}
}

func TestUserSessionStoreInboxFileWorkspaceAndValidation(t *testing.T) {
	r, _, p := startedRuntime(t)
	p.handler = inboxScriptHandler
	ws := userSessionWorkspaceDir("hivecommons", "hive")
	path, err := r.StoreInboxFile(testSessionID, ws, testInboxFileName, []byte("x"))
	if err != nil || path != filepath.Join(ws, ".hive", "inbox", testInboxFileName) {
		t.Fatalf("workspace seed = %q, %v", path, err)
	}
	for _, bad := range []string{"/etc", filepath.Join(userSessionWorkspaceRoot(), "only-org"), filepath.Join(ws, "sub"), filepath.Join(userSessionWorkspaceRoot(), "..", "x", "y")} {
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
// behind: mode 0640, the given group, a .gitignore, a replaced re-send, and a
// refusal at a symlinked .hive.
func TestUserSessionInboxScriptOnRealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ws := t.TempDir()
	gid := strconv.Itoa(os.Getgid())
	run := func(dir, content string) error {
		cmd := exec.Command("sh", "-c", userSessionInboxScript, "sh", dir, testInboxFileName, gid)
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
}
