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
	"testing"
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
