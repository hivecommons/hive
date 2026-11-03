package github

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitForFile polls until path exists or the deadline passes. Ticker-driven so
// it adds no time.Sleep to the test tree.
func waitForFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// The watcher loop itself: it returns immediately, picks up a request on its
// tick, and closes the done channel once ctx is cancelled so callers can join.
func TestStartPushBranchRequestWatcher_ProcessesOnTickAndJoins(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)
	oldInterval := pushBranchRequestPollInterval
	pushBranchRequestPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pushBranchRequestPollInterval = oldInterval })

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/loop", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	authz := func(agent string, uid int) error { return nil }
	done := c.StartPushBranchRequestWatcher(ctx, authz, nil)
	if c.pushBranchAuthz == nil {
		t.Fatal("Start did not install the authorizer")
	}

	resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if !waitForFile(t, resultPath, 5*time.Second) {
		cancel()
		t.Fatalf("watcher never processed the request at %s", reqPath)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done channel did not close after cancel")
	}
	if resp := readPushBranchResult(t, reqPath); !resp.OK || resp.Branch != "scanner/loop" {
		t.Errorf("result = %+v, want a successful push", resp)
	}
	assertRequestDirMode(t, "push", dir)
}

// A nil client is a no-op whose done channel is already closed; a request dir
// that cannot be created (its parent is a regular file) likewise never spawns
// a loop.
func TestStartPushBranchRequestWatcher_NilClientAndUncreatableDir(t *testing.T) {
	var nilClient *Client
	done := nilClient.StartPushBranchRequestWatcher(context.Background(), nil, nil)
	select {
	case <-done:
	default:
		t.Fatal("nil client: done channel should be closed on return")
	}

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := pushBranchRequestDirForTest
	pushBranchRequestDirForTest = filepath.Join(parent, "push-requests")
	t.Cleanup(func() { pushBranchRequestDirForTest = old })

	c := pushTestClient(t, "http://127.0.0.1:1")
	done = c.StartPushBranchRequestWatcher(context.Background(), func(string, int) error { return nil }, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("uncreatable dir: watcher should return a closed done channel without looping")
	}
}

// pushBranchRequestDir falls back to the production path when no test override
// is set, and a nil client's one-shot entry point is a no-op.
func TestPushBranchRequestDir_DefaultAndNilClientOnce(t *testing.T) {
	old := pushBranchRequestDirForTest
	pushBranchRequestDirForTest = ""
	t.Cleanup(func() { pushBranchRequestDirForTest = old })
	if got := pushBranchRequestDir(); got != PushBranchRequestDir {
		t.Errorf("pushBranchRequestDir() = %q, want %q", got, PushBranchRequestDir)
	}
	var nilClient *Client
	nilClient.ProcessPushBranchRequestsOnce(context.Background()) // must not panic
}

// A missing request dir is not an error: the scan simply does nothing.
func TestProcessPushBranchRequests_MissingDirIsQuiet(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	old := pushBranchRequestDirForTest
	pushBranchRequestDirForTest = filepath.Join(t.TempDir(), "never-created")
	t.Cleanup(func() { pushBranchRequestDirForTest = old })

	c.ProcessPushBranchRequestsOnce(context.Background())
	if len(*calls) != 0 {
		t.Errorf("%d git calls with no request dir, want 0", len(*calls))
	}
}

// The real command runner: captures stdout and stderr together, runs in the
// requested dir with the extra env, and surfaces a non-zero exit as an error.
func TestRunPushBranchCommand_CapturesOutputEnvAndExit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	out, err := runPushBranchCommand(context.Background(), dir, []string{"HIVE_PUSH_TEST_MARK=marker-42"},
		"sh", "-c", `echo "out:$HIVE_PUSH_TEST_MARK"; echo "err-line" 1>&2; pwd; exit 3`)
	if err == nil {
		t.Fatal("exit 3 reported no error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Errorf("err = %v, want exit status 3", err)
	}
	for _, want := range []string{"out:marker-42", "err-line"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lost %q", out, want)
		}
	}
	wantDir, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(out, wantDir) {
		t.Errorf("command did not run in %s: %q", wantDir, out)
	}

	if out, err := runPushBranchCommand(context.Background(), dir, nil, "sh", "-c", "echo ok"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("successful command: out=%q err=%v", out, err)
	}
}

// gitAuthHeader prefers the App installation token, falls back to a static
// token, and reports no header for a nil client or a failed App mint.
func TestGitAuthHeader_Sources(t *testing.T) {
	var nilClient *Client
	if h, ok := nilClient.gitAuthHeader(context.Background()); ok || h != "" {
		t.Errorf("nil client returned header %q ok=%v", h, ok)
	}

	c := pushTestClient(t, "http://127.0.0.1:1")
	if h, ok := c.gitAuthHeader(context.Background()); !ok || h != gitBasicAuthHeader("fake") {
		t.Errorf("token client header = %q ok=%v, want the static token encoded", h, ok)
	}

	c.appAuth = &AppAuth{cachedToken: "ghs_cached", tokenExpiry: time.Now().Add(time.Hour)}
	if h, ok := c.gitAuthHeader(context.Background()); !ok || h != gitBasicAuthHeader("ghs_cached") {
		t.Errorf("app client header = %q ok=%v, want the cached installation token", h, ok)
	}
	if strings.Contains(gitBasicAuthHeader("ghs_cached"), "ghs_cached") {
		t.Error("header carries the raw token instead of its base64 form")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c.appAuth = &AppAuth{key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: srv.URL}
	if h, ok := c.gitAuthHeader(context.Background()); ok || h != "" {
		t.Errorf("failed mint returned header %q ok=%v, want none (never fall back to the static token)", h, ok)
	}

	c.appAuth = nil
	c.authToken = ""
	if h, ok := c.gitAuthHeader(context.Background()); ok || h != "" {
		t.Errorf("client with no credential returned header %q ok=%v", h, ok)
	}
}

func TestFirstOutputLines(t *testing.T) {
	in := "\r\n\n  first \r\n\n second\nthird\nfourth\n"
	if got := firstOutputLines(in, 3); got != "first\nsecond\nthird" {
		t.Errorf("firstOutputLines(…, 3) = %q", got)
	}
	if got := firstOutputLines("", 3); got != "" {
		t.Errorf("empty input gave %q", got)
	}
	if got := firstOutputLines("only\n", 5); got != "only" {
		t.Errorf("short input gave %q", got)
	}
}

// Policy gates ahead of any git call, each with its own result reason and the
// .denied quarantine: lane write allowlist, repo pause, malformed repo ref and
// an unusable checkout dir.
func TestPushBranchRequestWatcher_PolicyDenials(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(c *Client)
		req    PushBranchRequest
		reason string
		audit  string
	}{
		{
			name: "lane write allowlist refuses push_branch",
			setup: func(c *Client) {
				c.SetWriteAllowlistFunc(func(agent, op string) bool { return op != WriteOpPushBranch })
			},
			req:    PushBranchRequest{Repo: "o/r", Branch: "scanner/x", Agent: "scanner"},
			reason: "push_branch",
			audit:  AuditActionAgentWriteRefused,
		},
		{
			name:   "paused repo",
			setup:  func(c *Client) { c.SetRepoPausedFunc(func(repo string) bool { return repo == "o/r" }) },
			req:    PushBranchRequest{Repo: "o/r", Branch: "scanner/x", Agent: "scanner"},
			reason: "paused",
		},
		{
			name:   "malformed repo ref",
			setup:  func(c *Client) {},
			req:    PushBranchRequest{Repo: "o/<repo>", Branch: "scanner/x", Agent: "scanner"},
			reason: "not a repository",
		},
		{
			name:   "relative checkout dir",
			setup:  func(c *Client) {},
			req:    PushBranchRequest{Repo: "o/r", Branch: "scanner/x", Dir: "relative/checkout", Agent: "scanner"},
			reason: "absolute path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := pushTestClient(t, "http://127.0.0.1:1")
			tc.setup(c)
			calls := withPushExec(t, "cafe1234", nil, "")
			recs := captureAudit(c)
			dir := withPushDir(t)
			req := tc.req
			if req.Dir == "" {
				req.Dir = t.TempDir()
			}
			reqPath, err := WritePushBranchRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessPushBranchRequestsOnce(context.Background())

			if len(*calls) != 0 {
				t.Fatalf("%d git calls, want 0 — the gate must run before any git", len(*calls))
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("denied request not quarantined: %v", err)
			}
			resp := readPushBranchResult(t, reqPath)
			if resp.OK || !strings.HasPrefix(resp.Error, "authorization denied: ") || !strings.Contains(resp.Error, tc.reason) {
				t.Errorf("result = %+v, want denial mentioning %q", resp, tc.reason)
			}
			if _, pushed := findAudit(*recs, AuditActionAgentBranchPushed); pushed {
				t.Error("a denied push was audited as pushed")
			}
			if tc.audit != "" {
				if _, ok := findAudit(*recs, tc.audit); !ok {
					t.Errorf("expected %s audit entry, got %+v", tc.audit, *recs)
				}
			}
		})
	}
}

// A default-branch lookup failure is retryable, not a denial: the request
// stays, the result records the attempt, and no git runs. Fail closed, never
// open.
func TestPushBranchRequestWatcher_DefaultBranchLookupFailureRetries(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.pushBranchAuthz = func(string, int) error { return nil }
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/x", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(*calls) != 0 {
		t.Fatalf("%d git calls with an unknown default branch, want 0", len(*calls))
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Errorf("request should remain for retry: %v", err)
	}
	if resp := readPushBranchResult(t, reqPath); resp.OK || resp.Attempts != 1 || strings.HasPrefix(resp.Error, "authorization denied") {
		t.Errorf("result = %+v, want a retryable failure with attempts=1", resp)
	}
}

// A local branch that does not exist can never push: rev-parse's failure is
// recorded with its output and no push is attempted.
func TestPushBranchRequestWatcher_MissingLocalBranch(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	var calls []pushExecCall
	old := pushBranchExec
	pushBranchExec = func(_ context.Context, dir string, env []string, name string, args ...string) (string, error) {
		calls = append(calls, pushExecCall{dir: dir, env: env, args: append([]string{name}, args...)})
		return "fatal: Needed a single revision\n", errors.New("exit status 128")
	}
	t.Cleanup(func() { pushBranchExec = old })
	dir := withPushDir(t)

	reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/gone", Dir: t.TempDir(), Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())

	if len(calls) != 1 || calls[0].args[1] != "rev-parse" {
		t.Fatalf("calls = %+v, want exactly one rev-parse and no push", calls)
	}
	resp := readPushBranchResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, `local branch "scanner/gone" not found`) || !strings.Contains(resp.Error, "Needed a single revision") {
		t.Errorf("result = %+v, want the missing-branch reason with git's output", resp)
	}
}

// Malformed request files: a fresh unparseable file is treated as a torn
// write and left alone; a stale one is quarantined as .bad with a result.
func TestPushBranchRequestWatcher_InvalidJSONQuarantine(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)
	path := filepath.Join(dir, "scanner-1.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh: in-flight grace applies.
	c.processPushBranchRequests(context.Background(), time.Now)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh malformed file was not left for the next tick: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".json") + ".result.json"); !os.IsNotExist(err) {
		t.Error("a torn read must not produce a result file")
	}

	// Stale: the clock says the file stopped changing long ago.
	later := func() time.Time { return time.Now().Add(2 * inFlightGrace) }
	c.processPushBranchRequests(context.Background(), later)
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Errorf("stale malformed file not quarantined as .bad: %v", err)
	}
	if resp := readPushBranchResult(t, path); resp.OK || !strings.Contains(resp.Error, "invalid JSON") {
		t.Errorf("result = %+v, want invalid JSON", resp)
	}
	if len(*calls) != 0 {
		t.Errorf("%d git calls for malformed requests, want 0", len(*calls))
	}
}

// A request that is not a regular file (here a symlink) is rejected outright
// and renamed .rejected — the drop box never follows links.
func TestPushBranchRequestWatcher_RejectsUnsafeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink drop-box rejection is a unix check")
	}
	c := pushTestClient(t, "http://127.0.0.1:1")
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte(`{"repo":"o/r","branch":"scanner/x","agent":"scanner"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "scanner-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	c.ProcessPushBranchRequestsOnce(context.Background())

	if _, err := os.Lstat(link + ".rejected"); err != nil {
		t.Errorf("symlinked request not renamed .rejected: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("%d git calls for a rejected file, want 0", len(*calls))
	}
}

// Result bookkeeping: an unreadable or malformed prior result counts as zero
// attempts; WritePushBranchRequest surfaces a dir it cannot create.
func TestPushBranchResultBookkeeping(t *testing.T) {
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "scanner-1.json")
	if got := priorPushBranchAttempts(reqPath); got != 0 {
		t.Errorf("no prior result: attempts = %d, want 0", got)
	}
	if err := os.WriteFile(strings.TrimSuffix(reqPath, ".json")+".result.json", []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := priorPushBranchAttempts(reqPath); got != 0 {
		t.Errorf("malformed prior result: attempts = %d, want 0", got)
	}

	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePushBranchRequest(filepath.Join(parent, "sub"), PushBranchRequest{Repo: "o/r", Branch: "b", Agent: "scanner"}); err == nil {
		t.Error("WritePushBranchRequest under a regular file reported no error")
	}
}
