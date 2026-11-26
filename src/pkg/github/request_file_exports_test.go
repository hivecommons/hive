package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestFileExports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	if err := WriteRequestResult(path, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteRequestResult: %v", err)
	}
	data, err := ReadRequestFile(path)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("ReadRequestFile = %q, %v", data, err)
	}
	if uid := RequestFileOwnerUID(path); uid != os.Getuid() {
		t.Errorf("RequestFileOwnerUID = %d, want %d", uid, os.Getuid())
	}
	if !RequestFileTorn(path, time.Now()) {
		t.Error("a fresh file is not reported as possibly torn")
	}
	if RequestFileTorn(path, time.Now().Add(time.Hour)) {
		t.Error("an old file is still reported as possibly torn")
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if _, err := ReadRequestFile(link); !IsRequestFileRejected(err) {
		t.Errorf("symlink read err = %v, want a rejection", err)
	}
	if _, err := ReadRequestFile(filepath.Join(dir, "missing.json")); err == nil || IsRequestFileRejected(err) {
		t.Errorf("missing file err = %v, want a plain not-found error", err)
	}

	if InvalidPRHeadReason("fix-1") != "" || InvalidPRHeadReason("HEAD") == "" {
		t.Error("InvalidPRHeadReason does not match invalidHeadReason")
	}
}

func TestReservedLabelForAgentsExport(t *testing.T) {
	for _, l := range []string{"hold", "needs-human", "stage/010-actionable", "hive/x", "queue"} {
		if !ReservedLabelForAgents(l, "queue") {
			t.Errorf("%q not reserved", l)
		}
	}
	if ReservedLabelForAgents("kind/bug", "queue") {
		t.Error("kind/bug reserved")
	}
}

func TestPushBranchExports(t *testing.T) {
	if !ValidPushBranchName("fix/19916") || ValidPushBranchName("-rf") || ValidPushBranchName("a..b") {
		t.Error("ValidPushBranchName does not match validPushBranchName")
	}
	dir := t.TempDir()
	if err := ValidatePushBranchDir(dir, os.Getuid()); err != nil {
		t.Errorf("own dir refused: %v", err)
	}
	if err := ValidatePushBranchDir("relative", 0); err == nil {
		t.Error("relative dir accepted")
	}

	const remote = "https://fj.example/acme/widgets.git"
	env := strings.Join(PushBranchGitEnv(dir, remote, "tok"), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "core.hooksPath", "credential.helper", "http." + remote + ".extraHeader", gitBasicAuthHeader("tok")} {
		if !strings.Contains(env, want) {
			t.Errorf("push env lacks %q:\n%s", want, env)
		}
	}
	if env := strings.Join(PushBranchGitEnv(dir, remote, ""), "\n"); strings.Contains(env, "extraHeader") {
		t.Errorf("an empty token still adds a header:\n%s", env)
	}
	if got := FirstOutputLines("\n a \n\nb\nc\n", 2); got != "a\nb" {
		t.Errorf("FirstOutputLines = %q", got)
	}

	queue := filepath.Join(t.TempDir(), "push-requests")
	if !EnsureRequestDir(nil, "push", queue) {
		t.Fatal("EnsureRequestDir failed")
	}
	if fi, err := os.Stat(queue); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSticky == 0 {
		t.Fatalf("queue = %v, %v; want a sticky directory", fi, err)
	}
}
