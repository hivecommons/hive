package policies

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// testLogger returns a no-op slog.Logger suitable for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- policyFileToAgent ---

func TestPolicyFileToAgent(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Recognized suffixes are stripped
		{"scanner.md", "scanner"},
		{"ci-maintainer-policy.md", "ci-maintainer"},
		{"architect_policy.md", "architect"},
		{"outreach-claude.md", "outreach-claude"},

		// No recognized suffix — returned as-is (minus extension)
		{"plain.md", "plain"},
		{"my-agent.md", "my-agent"},

		// Path component should be ignored (filepath.Base is applied)
		{"some/dir/scanner.md", "scanner"},

		// Suffix must not eat the whole name (idx > 0 guard)
		// e.g. a file literally named "-policy.md" where stripping "-policy" leaves nothing
		{"-policy.md", "-policy"},

		// -policy suffix is stripped
		{"agent-policy.md", "agent"},

		// _policy variant
		{"helper_policy.md", "helper"},

		// -claude is no longer a recognized suffix (backend-neutral)
		{"builder-claude.md", "builder-claude"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			got := policyFileToAgent(tc.input)
			if got != tc.want {
				t.Errorf("policyFileToAgent(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

// --- loadPolicies ---

func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("writeTempFile %s: %v", name, err)
	}
}

// newWatcherWithDir creates a Watcher whose localDir is root and subPath is "".
// This is useful for loadPolicies tests where we control the directory directly.
func newWatcherWithDir(t *testing.T, localDir, subPath string) *Watcher {
	t.Helper()
	return NewWatcher("https://example.com/repo.git", "", subPath, localDir, 10*time.Minute, testLogger())
}

func TestLoadPolicies_BasicMapping(t *testing.T) {
	root := t.TempDir()

	// Write three policy files using different supported suffix styles.
	writeTempFile(t, root, "scanner.md", "scanner policy content")
	writeTempFile(t, root, "ci-maintainer-policy.md", "ci-maintainer policy content")
	writeTempFile(t, root, "architect_policy.md", "architect policy content")
	writeTempFile(t, root, "plain.md", "plain content")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	want := map[string]string{
		"scanner":   "scanner policy content",
		"ci-maintainer":  "ci-maintainer policy content",
		"architect": "architect policy content",
		"plain":     "plain content",
	}

	for agent, wantContent := range want {
		data, ok := w.GetPolicy(agent)
		if !ok {
			t.Errorf("GetPolicy(%q): not found", agent)
			continue
		}
		if string(data) != wantContent {
			t.Errorf("GetPolicy(%q) = %q; want %q", agent, string(data), wantContent)
		}
	}
}

func TestLoadPolicies_SkipsNonMdFiles(t *testing.T) {
	root := t.TempDir()

	writeTempFile(t, root, "agent.md", "real policy")
	writeTempFile(t, root, "notes.txt", "should be ignored")
	writeTempFile(t, root, "README", "also ignored")
	writeTempFile(t, root, "script.sh", "ignored too")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	all := w.AllPolicies()
	if len(all) != 1 {
		t.Errorf("expected 1 policy, got %d: %v", len(all), keysOf(all))
	}
	if _, ok := all["agent"]; !ok {
		t.Errorf("expected 'agent' key; got keys: %v", keysOf(all))
	}
}

func TestLoadPolicies_SkipsDirectories(t *testing.T) {
	root := t.TempDir()

	// Create a sub-directory with a .md file inside — the directory itself
	// should be skipped; its contents are not recursed into.
	subDir := filepath.Join(root, "subdir.md") // deliberately ends in ".md"
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTempFile(t, subDir, "inner.md", "should not be loaded")

	writeTempFile(t, root, "top.md", "top level policy")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	all := w.AllPolicies()
	if len(all) != 1 {
		t.Errorf("expected 1 policy, got %d: %v", len(all), keysOf(all))
	}
	if _, ok := all["top"]; !ok {
		t.Errorf("expected 'top' key; got: %v", keysOf(all))
	}
}

func TestLoadPolicies_SubPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "policies")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeTempFile(t, sub, "outreach.md", "outreach text")

	w := newWatcherWithDir(t, root, "policies")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	data, ok := w.GetPolicy("outreach")
	if !ok {
		t.Fatal("GetPolicy('outreach') not found")
	}
	if string(data) != "outreach text" {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestLoadPolicies_MissingDir(t *testing.T) {
	w := newWatcherWithDir(t, "/nonexistent/path", "")
	err := w.loadPolicies()
	if err == nil {
		t.Error("expected error for missing directory, got nil")
	}
}

func TestLoadPolicies_ReloadsOnSecondCall(t *testing.T) {
	root := t.TempDir()
	writeTempFile(t, root, "agent.md", "v1")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("first load: %v", err)
	}

	data, _ := w.GetPolicy("agent")
	if string(data) != "v1" {
		t.Fatalf("expected v1, got %q", string(data))
	}

	// Update the file and reload.
	writeTempFile(t, root, "agent.md", "v2")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("second load: %v", err)
	}

	data, _ = w.GetPolicy("agent")
	if string(data) != "v2" {
		t.Errorf("expected v2 after reload, got %q", string(data))
	}
}

// --- GetPolicy ---

func TestGetPolicy_ExistingAgent(t *testing.T) {
	root := t.TempDir()
	writeTempFile(t, root, "myagent.md", "hello policy")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	data, ok := w.GetPolicy("myagent")
	if !ok {
		t.Fatal("expected ok=true for existing agent")
	}
	if string(data) != "hello policy" {
		t.Errorf("data = %q; want %q", string(data), "hello policy")
	}
}

func TestGetPolicy_MissingAgent(t *testing.T) {
	w := newWatcherWithDir(t, t.TempDir(), "")
	// No loadPolicies call — policies map is empty.
	_, ok := w.GetPolicy("nonexistent")
	if ok {
		t.Error("expected ok=false for missing agent")
	}
}

// --- AllPolicies ---

func TestAllPolicies_ReturnsCopy(t *testing.T) {
	root := t.TempDir()
	writeTempFile(t, root, "alpha.md", "alpha content")
	writeTempFile(t, root, "beta-policy.md", "beta content")

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	snapshot := w.AllPolicies()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(snapshot))
	}

	// Mutate the returned map — internal store must be unaffected.
	delete(snapshot, "alpha")
	snapshot["injected"] = []byte("evil")

	internal := w.AllPolicies()
	if _, ok := internal["alpha"]; !ok {
		t.Error("mutation of returned map affected internal store (alpha was deleted)")
	}
	if _, ok := internal["injected"]; ok {
		t.Error("mutation of returned map affected internal store (injected key present)")
	}
	if len(internal) != 2 {
		t.Errorf("internal map length changed: got %d, want 2", len(internal))
	}
}

func TestAllPolicies_EmptyWhenNoneLoaded(t *testing.T) {
	w := newWatcherWithDir(t, t.TempDir(), "")
	all := w.AllPolicies()
	if len(all) != 0 {
		t.Errorf("expected empty map, got %v", all)
	}
}

// --- NewWatcher ---

func TestNewWatcher_FieldsSetCorrectly(t *testing.T) {
	const (
		repoURL      = "https://github.com/example/repo.git"
		subPath      = "agents/policies"
		localDir     = "/tmp/test-policies-dir"
		pollInterval = 5 * time.Minute
	)

	logger := testLogger()
	w := NewWatcher(repoURL, "", subPath, localDir, pollInterval, logger)

	if w == nil {
		t.Fatal("NewWatcher returned nil")
	}
	if w.repoURL != repoURL {
		t.Errorf("repoURL = %q; want %q", w.repoURL, repoURL)
	}
	if w.branch != defaultPolicyBranch {
		t.Errorf("branch = %q; want %q (default)", w.branch, defaultPolicyBranch)
	}
	if w.subPath != subPath {
		t.Errorf("subPath = %q; want %q", w.subPath, subPath)
	}
	if w.localDir != localDir {
		t.Errorf("localDir = %q; want %q", w.localDir, localDir)
	}
	if w.pollInterval != pollInterval {
		t.Errorf("pollInterval = %v; want %v", w.pollInterval, pollInterval)
	}
	if w.policies == nil {
		t.Error("policies map is nil; want initialized map")
	}
	if len(w.policies) != 0 {
		t.Errorf("policies map is not empty on construction; got %v", w.policies)
	}
	if w.logger != logger {
		t.Error("logger field not set to provided logger")
	}
}

// --- Concurrency ---

func TestGetPolicyAndAllPolicies_Concurrent(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.md", "b-policy.md", "c_policy.md"} {
		writeTempFile(t, root, name, name+" content")
	}

	w := newWatcherWithDir(t, root, "")
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				w.GetPolicy("a")
			} else {
				w.AllPolicies()
			}
		}()
	}

	wg.Wait()
	// If the test reaches here without data race (run with -race), it passes.
}

// --- loadPolicies: unreadable file branch ---

func TestLoadPolicies_UnreadableFileIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is not meaningful on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}

	root := t.TempDir()
	writeTempFile(t, root, "readable.md", "good content")

	// Write a file then make it unreadable.
	unreadable := filepath.Join(root, "secret.md")
	if err := os.WriteFile(unreadable, []byte("secret"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(unreadable, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0644) }) // restore so TempDir cleanup works

	w := newWatcherWithDir(t, root, "")
	// loadPolicies should NOT return an error — it logs a warning and continues.
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: unexpected error: %v", err)
	}

	all := w.AllPolicies()
	// Only the readable file should be in the map.
	if _, ok := all["readable"]; !ok {
		t.Error("expected 'readable' policy to be loaded")
	}
	if _, ok := all["secret"]; ok {
		t.Error("expected 'secret' policy to be absent (unreadable file)")
	}
	if len(all) != 1 {
		t.Errorf("expected 1 policy, got %d: %v", len(all), keysOf(all))
	}
}

// --- pollLoop ---

func TestPollLoop_ExitsOnContextCancel(t *testing.T) {
	// Use a very long poll interval so the ticker never fires during the test.
	// We only want to verify the ctx.Done() branch terminates the goroutine.
	w := newWatcherWithDir(t, t.TempDir(), "")
	w.pollInterval = 24 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.pollLoop(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// pollLoop exited correctly.
	case <-time.After(2 * time.Second):
		t.Error("pollLoop did not exit within 2s after context cancel")
	}
}

// --- git-backed tests (Start, initialClone, pull) ---

// setupBareRepo creates a bare git repo, clones it to a working copy, writes
// policy files, commits and pushes them, then returns the file:// URL of the
// bare repo and a cleanup function.
//
// The caller receives:
//
//	bareURL  – file:///path/to/bare  (pass as repoURL to NewWatcher)
//	workDir  – the working clone (use for adding new commits later)
//
// The default branch is always "main", regardless of the system's
// init.defaultBranch setting.
func setupBareRepo(t *testing.T) (bareURL string, workDir string) {
	t.Helper()

	base := t.TempDir()
	bareDir := filepath.Join(base, "bare.git")
	workDir = filepath.Join(base, "work")

	// ---- create bare repo with an explicit default branch of "main" ----
	runGit(t, "", "init", "--bare", "--initial-branch=main", bareDir)

	// ---- clone it so we can add an initial commit ----
	runGit(t, "", "clone", bareDir, workDir)

	// git requires user identity for commits; set them locally so the test is
	// hermetic and works in CI where no global config exists.
	runGit(t, workDir, "config", "user.email", "test@example.com")
	runGit(t, workDir, "config", "user.name", "Test")

	// Ensure the local branch is named "main" to match the watcher's
	// hard-coded "origin main" argument.
	runGit(t, workDir, "checkout", "-B", "main")

	// ---- write policy files and commit ----
	policiesDir := filepath.Join(workDir, "policies")
	if err := os.MkdirAll(policiesDir, 0755); err != nil {
		t.Fatalf("mkdir policies: %v", err)
	}
	writeFile(t, policiesDir, "scanner.md", "scanner policy v1")
	writeFile(t, policiesDir, "ci-maintainer-policy.md", "ci-maintainer policy v1")

	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "initial policies")
	runGit(t, workDir, "push", "--set-upstream", "origin", "main")

	bareURL = "file://" + bareDir
	return bareURL, workDir
}

// addCommit writes a new policy file to workDir, commits it, and pushes to
// origin main.  Use this to simulate an upstream update between pull cycles.
func addCommit(t *testing.T, workDir, filename, content string) {
	t.Helper()
	policiesDir := filepath.Join(workDir, "policies")
	writeFile(t, policiesDir, filename, content)
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "add "+filename)
	runGit(t, workDir, "push", "--set-upstream", "origin", "main")
}

// runGit runs a git command, failing the test immediately if it errors.
// Pass dir="" to run in the current working directory without -C.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	var cmd *exec.Cmd
	if dir != "" {
		full := append([]string{"-C", dir}, args...)
		cmd = exec.Command("git", full...)
	} else {
		cmd = exec.Command("git", args...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

// writeFile is a test-helper wrapper around os.WriteFile.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}

// TestStart_FreshClone verifies that Start() clones the repo from a
// file:// URL, loads the policy files that are in it, and leaves the
// watcher in a usable state.
func TestStart_FreshClone(t *testing.T) {
	bareURL, _ := setupBareRepo(t)

	localDir := filepath.Join(t.TempDir(), "clone")
	w := NewWatcher(bareURL, "", "policies", localDir, 24*time.Hour, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both policy files written in setupBareRepo must be present.
	for agent, want := range map[string]string{
		"scanner":  "scanner policy v1",
		"ci-maintainer": "ci-maintainer policy v1",
	} {
		data, ok := w.GetPolicy(agent)
		if !ok {
			t.Errorf("GetPolicy(%q): not found after Start", agent)
			continue
		}
		if string(data) != want {
			t.Errorf("GetPolicy(%q) = %q; want %q", agent, string(data), want)
		}
	}
}

// TestStart_PollPicksUpNewCommit verifies the poll loop: after a new commit
// is pushed to the bare repo, the next poll should cause pull() to fetch it
// and reload the policies.
func TestStart_PollPicksUpNewCommit(t *testing.T) {
	bareURL, workDir := setupBareRepo(t)

	localDir := filepath.Join(t.TempDir(), "clone")

	// Use a short poll interval so the test completes quickly.
	const pollInterval = 200 * time.Millisecond
	w := NewWatcher(bareURL, "", "policies", localDir, pollInterval, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify baseline — new agent not yet present.
	if _, ok := w.GetPolicy("builder"); ok {
		t.Fatal("builder policy should not exist yet")
	}

	// Push a new commit to the bare repo.
	addCommit(t, workDir, "builder.md", "builder policy v1")

	// Wait up to 5 s for the poll loop to pick it up.
	const (
		maxWait     = 5 * time.Second
		checkPeriod = 50 * time.Millisecond
	)
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if data, ok := w.GetPolicy("builder"); ok {
			if string(data) == "builder policy v1" {
				return // success
			}
		}
		time.Sleep(checkPeriod)
	}

	t.Error("builder policy was not loaded after polling for 5 s")
}

// TestInitialClone_ExistingValidRepo verifies the branch in initialClone
// that detects an existing .git directory and runs git pull instead of
// git clone.
func TestInitialClone_ExistingValidRepo(t *testing.T) {
	bareURL, workDir := setupBareRepo(t)

	// Pre-clone the repo into localDir so .git already exists.
	localDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", bareURL, localDir)
	runGit(t, localDir, "config", "user.email", "test@example.com")
	runGit(t, localDir, "config", "user.name", "Test")

	// Push a new commit so that git pull has something to fetch.
	addCommit(t, workDir, "architect_policy.md", "architect policy v1")

	w := NewWatcher(bareURL, "", "policies", localDir, 24*time.Hour, testLogger())
	if err := w.initialClone(); err != nil {
		t.Fatalf("initialClone on existing repo: %v", err)
	}

	// After initialClone the new commit must be present locally.
	content, err := os.ReadFile(filepath.Join(localDir, "policies", "architect_policy.md"))
	if err != nil {
		t.Fatalf("architect_policy.md not present after pull: %v", err)
	}
	if string(content) != "architect policy v1" {
		t.Errorf("architect_policy.md content = %q; want %q", string(content), "architect policy v1")
	}
}

// TestInitialClone_CorruptDotGitTriggersReclone verifies the fallback path
// in initialClone: if a .git directory exists but git pull fails (simulated
// by corrupting .git), the watcher removes the broken clone and re-clones
// from the remote.
func TestInitialClone_CorruptDotGitTriggersReclone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.RemoveAll of locked .git is unreliable on Windows")
	}

	bareURL, _ := setupBareRepo(t)

	localDir := filepath.Join(t.TempDir(), "clone")

	// Create a fake .git directory that will make git pull fail.
	fakeGit := filepath.Join(localDir, ".git")
	if err := os.MkdirAll(fakeGit, 0755); err != nil {
		t.Fatalf("mkdir fake .git: %v", err)
	}
	// Write a corrupt HEAD so git treats this as a git repo but fails immediately.
	if err := os.WriteFile(filepath.Join(fakeGit, "HEAD"), []byte("not-a-valid-ref\n"), 0644); err != nil {
		t.Fatalf("write corrupt HEAD: %v", err)
	}

	w := NewWatcher(bareURL, "", "policies", localDir, 24*time.Hour, testLogger())
	if err := w.initialClone(); err != nil {
		t.Fatalf("initialClone with corrupt .git: %v", err)
	}

	// The re-clone must have succeeded — the policies dir must exist.
	entries, err := os.ReadDir(filepath.Join(localDir, "policies"))
	if err != nil {
		t.Fatalf("policies dir not present after re-clone: %v", err)
	}
	if len(entries) == 0 {
		t.Error("policies dir is empty after re-clone; expected policy files")
	}
}

// TestPull_UpdatesPoliciesToLatest verifies pull() in isolation: seed the
// local clone, push a new commit to the bare repo, then call pull() directly
// and check that loadPolicies picks up the new file.
func TestPull_UpdatesPoliciesToLatest(t *testing.T) {
	bareURL, workDir := setupBareRepo(t)

	// Clone into localDir ourselves so we control the state before pull().
	localDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", bareURL, localDir)
	runGit(t, localDir, "config", "user.email", "test@example.com")
	runGit(t, localDir, "config", "user.name", "Test")

	w := NewWatcher(bareURL, "", "policies", localDir, 24*time.Hour, testLogger())

	// Load the initial state.
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("initial loadPolicies: %v", err)
	}

	// Verify builder is not yet present.
	if _, ok := w.GetPolicy("builder"); ok {
		t.Fatal("builder should not exist before new commit")
	}

	// Push a new policy file.
	addCommit(t, workDir, "builder.md", "builder policy v2")

	// Call pull() — it should git pull and reload.
	w.pull()

	data, ok := w.GetPolicy("builder")
	if !ok {
		t.Fatal("builder policy not found after pull()")
	}
	if string(data) != "builder policy v2" {
		t.Errorf("builder policy = %q; want %q", string(data), "builder policy v2")
	}
}

// TestPull_AlreadyUpToDate verifies that pull() does NOT reload policies
// when the repo is already up to date (the "Already up to date." fast-path).
func TestPull_AlreadyUpToDate(t *testing.T) {
	bareURL, _ := setupBareRepo(t)

	localDir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", bareURL, localDir)
	runGit(t, localDir, "config", "user.email", "test@example.com")
	runGit(t, localDir, "config", "user.name", "Test")

	w := NewWatcher(bareURL, "", "policies", localDir, 24*time.Hour, testLogger())
	if err := w.loadPolicies(); err != nil {
		t.Fatalf("loadPolicies: %v", err)
	}

	// Record the policy map before the no-op pull.
	before := w.AllPolicies()

	// pull() with nothing new — should be a no-op.
	w.pull()

	after := w.AllPolicies()
	if len(before) != len(after) {
		t.Errorf("policy count changed after up-to-date pull: %d → %d", len(before), len(after))
	}
}

// --- helpers ---

func keysOf(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
