package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent-facing request queues must exist even when no watcher is running.
//
// The PR/issue watchers are gated on a usable GitHub App, and the design says
// that with no App "requests simply accumulate rather than opening under a
// wrong identity". That was not true: the queues were created INSIDE the same
// gate, so on a hive whose App was not usable at boot the directories did not
// exist and hive-open-pr / hive-open-issue hard-failed in the agent's shell —
// the finding was discarded, not queued, and the only trace was in that
// agent's terminal pane.
//
// Measured on a live L3 hive: the App installation ID was saved from the
// dashboard AFTER boot, so auto-discovery repaired App auth at runtime and
// everything reported healthy — while quality had analysed the repo, pushed a
// branch, composed a PR and an issue, and then had nowhere to put them.

func testQueueDirs(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	pr := filepath.Join(root, "pr-requests")
	issue := filepath.Join(root, "issue-requests")
	prev, prevIssue := prRequestDirForTest, issueRequestDirForTest
	prRequestDirForTest, issueRequestDirForTest = pr, issue
	t.Cleanup(func() { prRequestDirForTest, issueRequestDirForTest = prev, prevIssue })
	return pr, issue
}

// TestPrepareRequestDirsCreatesBothQueues is the regression: both queues exist
// after PrepareRequestDirs, with no client and no App anywhere in sight.
func TestPrepareRequestDirsCreatesBothQueues(t *testing.T) {
	pr, issue := testQueueDirs(t)

	PrepareRequestDirs(quietLogger())

	for name, dir := range map[string]string{"pr": pr, "issue": issue} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s queue was not created: %v — agents hard-fail instead of queueing", name, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s queue is not a directory", name)
		}
	}
}

// TestPrepareRequestDirsIsAgentWritable pins the permissions agents depend on.
// MkdirAll is umask-masked to 0755; without the explicit chmod an agent's drop
// gets EACCES and the write is lost exactly as if the dir were missing.
func TestPrepareRequestDirsIsAgentWritable(t *testing.T) {
	pr, issue := testQueueDirs(t)

	PrepareRequestDirs(quietLogger())

	for name, dir := range map[string]string{"pr": pr, "issue": issue} {
		assertRequestDirMode(t, name, dir)
	}
}

func TestPrepareRequestDirsUpgradesExistingQueueModes(t *testing.T) {
	pr, issue := testQueueDirs(t)

	for name, dir := range map[string]string{"pr": pr, "issue": issue} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("%s queue pre-create: %v", name, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("%s queue chmod: %v", name, err)
		}
		queued := filepath.Join(dir, "pending.json")
		if err := os.WriteFile(queued, []byte(`{"agent":"quality"}`), 0o664); err != nil {
			t.Fatalf("%s queue write queued request: %v", name, err)
		}
	}

	PrepareRequestDirs(quietLogger())

	for name, dir := range map[string]string{"pr": pr, "issue": issue} {
		assertRequestDirMode(t, name, dir)
		if _, err := os.Stat(filepath.Join(dir, "pending.json")); err != nil {
			t.Fatalf("%s queued request was lost across permission upgrade: %v", name, err)
		}
	}
}

// TestPrepareRequestDirsIsIdempotent: it runs on every boot, and an existing
// queue holding queued requests must survive untouched.
func TestPrepareRequestDirsIsIdempotent(t *testing.T) {
	pr, _ := testQueueDirs(t)
	if err := os.MkdirAll(pr, 0o2775); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	queued := filepath.Join(pr, "pending.json")
	if err := os.WriteFile(queued, []byte(`{"agent":"quality"}`), 0o664); err != nil {
		t.Fatalf("write queued request: %v", err)
	}

	PrepareRequestDirs(quietLogger())

	if _, err := os.Stat(queued); err != nil {
		t.Fatalf("a queued request was lost across PrepareRequestDirs: %v", err)
	}
}

func assertRequestDirMode(t *testing.T, name, dir string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("%s queue: %v", name, err)
	}
	if perm := info.Mode().Perm(); perm&0o070 != 0o070 {
		t.Errorf("%s queue perm = %#o, want group rwx — agents cannot drop request files", name, perm)
	}
	if info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("%s queue missing setgid bit — agent-written files won't inherit the node group", name)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Errorf("%s queue missing sticky bit — any agent could unlink a peer's queued request", name)
	}
}

// TestEnsureRequestDirReportsFailure: the watcher disables itself only when the
// queue genuinely cannot be created, so that signal must stay truthful.
func TestEnsureRequestDirReportsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — an unwritable parent is still writable")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	if ensureRequestDir(quietLogger(), "pr", filepath.Join(parent, "nope")) {
		t.Error("ensureRequestDir reported success for an uncreatable directory")
	}
}

// TestRequestWatchersStayGatedInMain pins the refusal half of the contract at
// the source level, the layer no behavioural test in this package can see
// (shape follows f3_trusted_merger_source_test.go, and for the same reason: a
// sync merge resolving a conflict in favour of an older main.go could ungate
// the watchers while every test here stays green).
//
// PrepareRequestDirs only makes the queues EXIST. Acting on a queued request —
// opening a PR or issue on GitHub — must remain gated on a usable App: with no
// App there is no bot identity to author as, and requests must accumulate on
// disk, never open under a wrong identity.
func TestRequestWatchersStayGatedInMain(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/hive/main.go")
	if err != nil {
		t.Fatalf("reading cmd/hive/main.go: %v", err)
	}
	src := string(raw)

	gate := "if ghClient != nil && cfg.GitHub.HasUsableApp() {"
	gateIdx := strings.Index(src, gate)
	if gateIdx < 0 {
		t.Fatalf("cmd/hive/main.go lost the usable-App gate %q — the request watchers "+
			"must never start without an App identity to author as", gate)
	}

	prepIdx := strings.Index(src, "github.PrepareRequestDirs(")
	if prepIdx < 0 {
		t.Error("cmd/hive/main.go no longer calls github.PrepareRequestDirs — queues " +
			"stop existing on App-less boots and agent findings are discarded again (#4713)")
	} else if prepIdx > gateIdx {
		t.Error("github.PrepareRequestDirs must be called BEFORE (outside) the usable-App " +
			"gate — inside it, queues stop existing on App-less boots (#4713)")
	}

	for _, call := range []string{"requestwatch.New(", "startRequestWatchers("} {
		idx := strings.Index(src, call)
		if idx < 0 {
			t.Errorf("cmd/hive/main.go no longer starts %s — queued requests would accumulate forever", call)
			continue
		}
		if idx < gateIdx {
			t.Errorf("%s appears before the usable-App gate — a watcher started without a "+
				"usable App could act on GitHub without the bot identity", call)
		}
	}
}
