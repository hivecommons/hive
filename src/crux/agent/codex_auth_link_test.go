package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// #20002: the codex auth link step must never replace or delete a regular
// auth.json at the per-agent path. These tests run the real link script on
// synthetic files only (no su-exec, no credential content).

func runCodexAuthLinkForTest(t *testing.T, shared, link string) bool {
	t.Helper()
	kept, err := linkCodexAuth(exec.Command("sh", "-c", codexAuthLinkScript, "sh", shared, link))
	if err != nil {
		t.Fatalf("linkCodexAuth: %v", err)
	}
	return kept
}

func codexAuthTestPaths(t *testing.T) (shared, link string) {
	t.Helper()
	dir := t.TempDir()
	sharedDir := filepath.Join(dir, "shared")
	agentDir := filepath.Join(dir, "agent")
	for _, d := range []string{sharedDir, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(sharedDir, "auth.json"), filepath.Join(agentDir, "auth.json")
}

func assertLinksTo(t *testing.T, link, want string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", link, info.Mode())
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s links to %q, want %q", link, got, want)
	}
}

func TestLinkCodexAuth_AbsentPathGetsLink(t *testing.T) {
	shared, link := codexAuthTestPaths(t)
	if kept := runCodexAuthLinkForTest(t, shared, link); kept {
		t.Fatal("absent path reported as kept")
	}
	assertLinksTo(t, link, shared)
}

func TestLinkCodexAuth_ExistingLinkIsRepointed(t *testing.T) {
	shared, link := codexAuthTestPaths(t)
	stale := filepath.Join(filepath.Dir(link), "elsewhere.json")
	if err := os.Symlink(stale, link); err != nil {
		t.Fatal(err)
	}
	if kept := runCodexAuthLinkForTest(t, shared, link); kept {
		t.Fatal("existing symlink reported as kept")
	}
	assertLinksTo(t, link, shared)
}

func TestLinkCodexAuth_DanglingLinkIsRelinked(t *testing.T) {
	shared, link := codexAuthTestPaths(t)
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}
	// shared does not exist: the link is dangling, which is the normal state
	// before the first login.
	if kept := runCodexAuthLinkForTest(t, shared, link); kept {
		t.Fatal("dangling symlink reported as kept")
	}
	assertLinksTo(t, link, shared)
}

// The invariant from #20002: a regular file at the link path survives the
// link step byte-for-byte, and the shared file is not touched.
func TestLinkCodexAuth_RegularFileIsKeptUntouched(t *testing.T) {
	shared, link := codexAuthTestPaths(t)
	const sharedContent = "synthetic-shared-placeholder"
	const privateContent = "synthetic-private-placeholder"
	if err := os.WriteFile(shared, []byte(sharedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(privateContent), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(link)
	if err != nil {
		t.Fatal(err)
	}

	if kept := runCodexAuthLinkForTest(t, shared, link); !kept {
		t.Fatal("regular file not reported as kept")
	}

	after, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("regular auth.json was removed: %v", err)
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() {
		t.Fatalf("regular auth.json was replaced (mode %v)", after.Mode())
	}
	if !os.SameFile(before, after) {
		t.Fatal("regular auth.json was replaced by a different file")
	}
	if got, _ := os.ReadFile(link); string(got) != privateContent {
		t.Fatalf("private file content changed: %q", got)
	}
	if got, _ := os.ReadFile(shared); string(got) != sharedContent {
		t.Fatalf("shared file content changed: %q", got)
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("private file mode changed to %v", after.Mode().Perm())
	}
}

func TestLinkCodexAuth_DirectoryIsKept(t *testing.T) {
	shared, link := codexAuthTestPaths(t)
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if kept := runCodexAuthLinkForTest(t, shared, link); !kept {
		t.Fatal("directory at link path not reported as kept")
	}
	entries, err := os.ReadDir(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("link step wrote into the directory: %v", entries)
	}
}

func TestLinkCodexAuth_OtherFailureIsAnError(t *testing.T) {
	kept, err := linkCodexAuth(exec.Command("sh", "-c", "exit 1"))
	if err == nil {
		t.Fatal("expected an error for a non-kept failure")
	}
	if kept {
		t.Fatal("a failure must not be reported as kept")
	}
}

func TestCodexAuthLinkCommand_RunsScriptAsAgent(t *testing.T) {
	cmd := codexAuthLinkCommand("agent:agent", "/shared/auth.json", "/home/a/auth.json")
	want := []string{"su-exec", "agent:agent", "sh", "-c", codexAuthLinkScript, "sh", "/shared/auth.json", "/home/a/auth.json"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}
