package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newMergeMockServer mocks PUT /pulls/{n}/merge. When failWith != 0 it returns
// that status (to exercise the failure/retry path); otherwise 200 merged.
// merges counts successful PUT /merge calls.
func newMergeMockServer(t *testing.T, failWith int, merges *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge"):
			if failWith != 0 {
				w.WriteHeader(failWith)
				_, _ = io.WriteString(w, `{"message":"Pull Request is not mergeable"}`)
				return
			}
			if merges != nil {
				*merges++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/update-branch"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"message":"Updating pull request branch."}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func testMergeClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := NewClientForTest(srvURL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error { return nil }
	return c
}

func readMergeResult(t *testing.T, reqPath string) MergeResponse {
	t.Helper()
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading result %s: %v", out, err)
	}
	var resp MergeResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("bad result json: %v", err)
	}
	return resp
}

func TestMergeRequestWatcherSetsQueueMode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		preexist bool
	}{
		{name: "fresh"},
		{name: "upgrade existing", preexist: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "merge-requests")
			if tt.preexist {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("pre-create merge queue: %v", err)
				}
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatalf("pre-create chmod: %v", err)
				}
			}
			mergeRequestDirForTest = dir
			t.Cleanup(func() { mergeRequestDirForTest = "" })

			ctx, cancel := context.WithCancel(context.Background())
			done := testMergeClient(t, "http://127.0.0.1:0").StartMergeRequestWatcher(ctx, nil, nil)
			cancel()
			<-done

			assertRequestDirMode(t, "merge", dir)
		})
	}
}

// End-to-end: a merge request file is merged over REST, consumed, result written.
func TestMergeRequestWatcher_MergesAndConsumes(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Method: "squash", Agent: "scanner", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 1 {
		t.Fatalf("expected 1 merge, got %d", merges)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("request file should be consumed (removed) after a successful merge")
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK || resp.SHA != "deadbeef" {
		t.Errorf("expected ok result with merge sha, got %+v", resp)
	}
}

// A nil authorizer must fail closed — no merge, request quarantined .denied.
func TestMergeRequestWatcher_NilAuthzFailsClosed(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// c.mergeAuthz intentionally nil

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Agent: "scanner"})
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("nil authorizer must not merge, got %d merges", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Error("denied request should be quarantined as .denied")
	}
}

// An authorizer that denies quarantines the request without merging.
func TestMergeRequestWatcher_AuthzDeny(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error {
		return context.DeadlineExceeded
	}

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7, Agent: "quality"})
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("denied request must not merge, got %d", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Error("denied request should be quarantined as .denied")
	}
}

// F4: the authorizer receives the merge TARGET (repo, number, expectSHA), and a
// denial keyed on any of those (e.g. empty expectSHA or a target not in the
// eligible list) quarantines the request WITHOUT merging. This asserts the
// target-binding parameters actually reach the authorizer.
func TestMergeRequestWatcher_TargetBoundAuthz(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var gotRepo, gotSHA string
	var gotNumber int
	// Stand-in for bindMergeAuthz's SHA guard: deny when expectSHA is empty.
	c.mergeAuthz = func(agent string, uid int, repo string, number int, expectSHA string) error {
		gotRepo, gotNumber, gotSHA = repo, number, expectSHA
		if strings.TrimSpace(expectSHA) == "" {
			return context.DeadlineExceeded // stand-in denial (TOCTOU guard)
		}
		return nil
	}

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	// No ExpectSHA → must be denied and quarantined, never merged.
	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 99, Agent: "scanner"})
	c.ProcessMergeRequestsOnce(context.Background())

	if gotRepo != "o/r" || gotNumber != 99 || gotSHA != "" {
		t.Errorf("authorizer did not receive the target: repo=%q number=%d sha=%q", gotRepo, gotNumber, gotSHA)
	}
	if merges != 0 {
		t.Fatalf("unpinned (empty-SHA) merge must be denied, got %d merges", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Error("target-bound denial should quarantine the request as .denied")
	}

	// With a pinned SHA the same target authorizes and merges.
	merges = 0
	reqPath2, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 100, Agent: "scanner", ExpectSHA: "abc123"})
	c.ProcessMergeRequestsOnce(context.Background())
	if gotSHA != "abc123" || gotNumber != 100 {
		t.Errorf("authorizer did not receive pinned target: number=%d sha=%q", gotNumber, gotSHA)
	}
	if merges != 1 {
		t.Fatalf("pinned + authorized merge should proceed, got %d merges", merges)
	}
	if _, err := os.Stat(reqPath2); !os.IsNotExist(err) {
		t.Error("successful merge should consume the request file")
	}
}

// A failing merge is retried, and after max attempts the request is exhausted.
func TestMergeRequestWatcher_RetryThenExhaust(t *testing.T) {
	srv := newMergeMockServer(t, http.StatusMethodNotAllowed, nil) // 405: not mergeable
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Agent: "scanner"})

	// Run up to max attempts; the request should remain (retryable) until the
	// final attempt quarantines it as .exhausted.
	for i := 0; i < mergeRequestMaxAttempts; i++ {
		c.ProcessMergeRequestsOnce(context.Background())
	}

	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Errorf("after %d failed attempts the request should be .exhausted", mergeRequestMaxAttempts)
	}
	// original request should no longer be present as a live .json
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("exhausted request should have been renamed away")
	}
}

// A malformed JSON request is quarantined as .bad without a merge attempt.
func TestMergeRequestWatcher_BadJSONQuarantined(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	bad := filepath.Join(dir, "scanner-1.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("bad request must not merge")
	}
	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Error("malformed request should be quarantined as .bad")
	}
}

func TestMergeRequestWatcher_RejectsSymlinkRequest(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"repo":"o/r","number":42,"agent":"scanner"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(dir, "scanner-1.json")
	if err := os.Symlink(target, request); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatal("symlink request must not merge")
	}
	if _, err := os.Lstat(request + ".bad"); err != nil {
		t.Fatalf("symlink request should be quarantined as .bad: %v", err)
	}
}

// --- Fix #2: re-engagement on a failing-required-check terminal merge failure ---

func TestIsRequiredCheckMergeBlocker(t *testing.T) {
	// GENERIC: these key off GitHub's branch-protection vocabulary only, never a
	// specific check/linter/language name.
	reEngage := []string{
		"Required status check \"x\" is expected",
		"At least 1 approving review is required by reviewers ... required status checks have not",
		"Changes must be made through a pull request",
		"the base branch requires all commits to be signed ... has not succeeded",
		`merging PR o/r#42 (squash): PUT http://127.0.0.1:64031/repos/o/r/pulls/42/merge: 405 Required status check "build" is expected.`,
	}
	for _, m := range reEngage {
		if !isRequiredCheckMergeBlocker(m) {
			t.Errorf("expected re-engageable required-check blocker: %q", m)
		}
	}
	unfixable := []string{
		"Pull Request is not mergeable", // conflict-family, not a check
		"merge conflict between base and head",
		"Resource not accessible by integration",
		"403 Forbidden: you do not have permission",
		"GitHub API returned status code: 403",
	}
	for _, m := range unfixable {
		if isRequiredCheckMergeBlocker(m) {
			t.Errorf("unfixable blocker must NOT re-engage: %q", m)
		}
	}
}

// requiredCheckMergeServer always fails PUT /merge with a required-check message.
func requiredCheckMergeServer(t *testing.T, attempts *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge") {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = io.WriteString(w, `{"message":"Required status check \"build\" is expected."}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestMergeWatcher_ReEngagesOnRequiredCheckFailure(t *testing.T) {
	var mergeAttempts atomic.Int32
	srv := requiredCheckMergeServer(t, &mergeAttempts)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	dir := t.TempDir()

	var reEngagedRepo string
	var reEngagedNum int
	var calls atomic.Int32
	c.SetMergeReEngageHook(func(repo string, number int) bool {
		calls.Add(1)
		reEngagedRepo, reEngagedNum = repo, number
		return true
	})

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	now := func() time.Time { return time.Unix(1700000000, 0) }
	// Drive the same request to the terminal attempt. Calling the handler
	// directly keeps this regression focused on the terminal merge-failure
	// re-engagement path instead of coupling it to the package-level test
	// directory override used by the scan-loop tests above.
	for i := 0; i < mergeRequestMaxAttempts; i++ {
		c.handleOneMergeRequest(context.Background(), reqPath, now)
	}

	if got := mergeAttempts.Load(); got != int32(mergeRequestMaxAttempts) {
		t.Fatalf("server should see %d merge attempts, got %d", mergeRequestMaxAttempts, got)
	}
	resp := readMergeResult(t, reqPath)
	if resp.Attempts != mergeRequestMaxAttempts {
		t.Fatalf("result should record terminal attempt %d, got %+v", mergeRequestMaxAttempts, resp)
	}
	if !isRequiredCheckMergeBlocker(resp.Error) {
		t.Fatalf("terminal error should be classified as a required-check blocker: %q", resp.Error)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("re-engage hook should fire exactly once at terminal failure, got %d (result=%+v)", got, resp)
	}
	if reEngagedRepo != "o/r" || reEngagedNum != 42 {
		t.Fatalf("hook got (%q,%d), want (o/r,42)", reEngagedRepo, reEngagedNum)
	}
	// The PR is quarantined (merge cannot succeed until green) but the fix loop
	// now owns it — a re-engaged terminal failure must NOT be silently dropped.
	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Fatalf("terminal request should be quarantined (.exhausted): %v", err)
	}
}

func TestMergeWatcher_UnfixableBlockerStillQuarantinesNoReEngage(t *testing.T) {
	// 405 "not mergeable" is the conflict-family message the base mock returns —
	// it must NOT trigger re-engagement.
	srv := newMergeMockServer(t, http.StatusMethodNotAllowed, nil)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	var calls int
	c.SetMergeReEngageHook(func(repo string, number int) bool { calls++; return true })

	reqPath, _ := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	for i := 0; i < mergeRequestMaxAttempts; i++ {
		c.ProcessMergeRequestsOnce(context.Background())
	}
	if calls != 0 {
		t.Fatalf("unfixable blocker must not re-engage the fix loop, got %d calls", calls)
	}
	if _, err := os.Stat(reqPath + ".exhausted"); err != nil {
		t.Fatalf("unfixable terminal failure should still quarantine: %v", err)
	}
}
