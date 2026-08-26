package github

import (
	"context"
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

// These tests cover the Start* watcher entry points (StartPRRequestWatcher,
// StartMergeRequestWatcher, StartReviewRequestWatcher) — the lifecycle wiring
// that boot calls. The per-request processing paths are covered by the
// Process*Once tests in the sibling files; here we pin the Start contracts:
// nil client is a no-op, the authorizer/hold-label hooks are stored on the
// client, the request dir is bootstrapped (or the watcher disables itself when
// it cannot be), and the ticker loop processes work and stops on ctx cancel.

// blockedRequestDir returns a path that MkdirAll cannot create (a child of a
// regular file), for exercising the disabled-on-uncreatable-dir path.
func blockedRequestDir(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "requests")
}

// --- StartPRRequestWatcher ---

// A nil client must be a silent no-op — the hive boots without GitHub creds as
// a nil *Client and calls Start unconditionally.
func TestPRRequestWatcher_StartNilClient(t *testing.T) {
	var c *Client
	c.StartPRRequestWatcher(context.Background(), nil, nil, nil) // must not panic
}

// Start must store the authorizer and hold-label hooks on the client and create
// the request dir, so that the (const-interval) ticker loop and the direct
// Process path both enforce the SAME policy that was handed to Start.
func TestPRRequestWatcher_StartWiresAuthzAndHold(t *testing.T) {
	created := 0
	var addedLabels []string
	srv := newPRMockServerLabels(t, "", &created, &addedLabels)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dir := filepath.Join(t.TempDir(), "pr-requests")
	old := prRequestDirForTest
	prRequestDirForTest = dir
	t.Cleanup(func() { prRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	authzCalled := false
	done := c.StartPRRequestWatcher(ctx, func(agent string, uid int) error {
		authzCalled = true
		return nil
	}, func() bool { return true }, nil)
	drainAfter(t, cancel, done)

	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("Start must create the request dir, stat err=%v", err)
	}

	// The stored hooks must drive processing: the request is authorized by the
	// authorizer passed to Start and the PR gets the hold label because the
	// hold hook passed to Start returned true.
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "b", Title: "t", Agent: "quality"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if !authzCalled {
		t.Error("authorizer passed to Start was never consulted")
	}
	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
	if !strings.Contains(strings.Join(addedLabels, ","), "hold") {
		t.Errorf("hold-label hook passed to Start was ignored, labels=%v", addedLabels)
	}
}

// When the request dir cannot be created the watcher disables itself without
// panicking (requests accumulate elsewhere; nothing is opened blindly).
func TestPRRequestWatcher_StartWithUncreatableDir(t *testing.T) {
	srv := newPRMockServer(t, "", nil)
	defer srv.Close()
	c := testClient(t, srv.URL)

	old := prRequestDirForTest
	prRequestDirForTest = blockedRequestDir(t)
	t.Cleanup(func() { prRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.StartPRRequestWatcher(ctx, nil, nil, nil) // must return without panicking
}

// --- StartMergeRequestWatcher ---

func TestMergeRequestWatcher_StartNilClient(t *testing.T) {
	var c *Client
	c.StartMergeRequestWatcher(context.Background(), nil, nil) // must not panic
}

// Start must create the request dir with group-writable perms (agents drop
// files here as their own UID) and store the merge authorizer.
func TestMergeRequestWatcher_StartWiresAuthzAndDir(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dir := filepath.Join(t.TempDir(), "merge-requests")
	old := mergeRequestDirForTest
	mergeRequestDirForTest = dir
	t.Cleanup(func() { mergeRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	authzCalled := false
	done := c.StartMergeRequestWatcher(ctx, func(agent string, uid int, repo string, number int, expectSHA string) error {
		authzCalled = true
		return nil
	}, nil)
	drainAfter(t, cancel, done)

	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("Start must create the request dir, stat err=%v", err)
	}
	if perm := st.Mode().Perm(); perm&0o070 != 0o070 {
		t.Errorf("request dir must be group-writable for agent drops, got perm %o", perm)
	}

	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7, Agent: "quality"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if !authzCalled {
		t.Error("authorizer passed to Start was never consulted")
	}
	if merges != 1 {
		t.Fatalf("expected 1 merge, got %d", merges)
	}
}

// Uncreatable dir → watcher disables itself, no panic, no goroutine.
func TestMergeRequestWatcher_StartWithUncreatableDir(t *testing.T) {
	srv := newMergeMockServer(t, 0, nil)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	old := mergeRequestDirForTest
	mergeRequestDirForTest = blockedRequestDir(t)
	t.Cleanup(func() { mergeRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.StartMergeRequestWatcher(ctx, nil, nil) // must return without panicking
}

// --- StartReviewRequestWatcher ---

func TestReviewRequestWatcher_StartNilClient(t *testing.T) {
	var c *Client
	c.StartReviewRequestWatcher(context.Background(), nil, nil) // must not panic
}

// Full lifecycle: the ticker loop (interval is a var, so we can shrink it)
// picks up a dropped request, submits the review through the authorizer passed
// to Start, and stops on ctx cancel.
func TestReviewRequestWatcher_StartLoop(t *testing.T) {
	var reviewed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews") {
			reviewed.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"state":"APPROVED"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dir := filepath.Join(t.TempDir(), "review-requests")
	oldDir := reviewRequestDirForTest
	reviewRequestDirForTest = dir
	t.Cleanup(func() { reviewRequestDirForTest = oldDir })

	oldInterval := reviewRequestPollInterval
	reviewRequestPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { reviewRequestPollInterval = oldInterval })

	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartReviewRequestWatcher(ctx, func(agent string, uid int) error { return nil }, nil)
	drainAfter(t, cancel, done)

	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("Start must create the request dir, stat err=%v", err)
	}
	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "approve", Agent: "quality",
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for reviewed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := reviewed.Load(); got != 1 {
		t.Fatalf("ticker loop never processed the request (reviewed=%d)", got)
	}
	cancel() // loop exit path
	time.Sleep(50 * time.Millisecond)
}

// Uncreatable dir → watcher disables itself, no panic.
func TestReviewRequestWatcher_StartWithUncreatableDir(t *testing.T) {
	srv := newReviewMockServer(t, nil, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)

	old := reviewRequestDirForTest
	reviewRequestDirForTest = blockedRequestDir(t)
	t.Cleanup(func() { reviewRequestDirForTest = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.StartReviewRequestWatcher(ctx, nil, nil) // must return without panicking
}
