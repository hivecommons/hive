package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pr/merge/review request watchers share a file-drop protocol with three
// seams the rest of the suite always overrides or never exercises: the
// production request-dir defaults (every other test sets the *ForTest var),
// the nil-receiver guards on the ProcessXRequestsOnce entry points and
// SetMergeReEngageHook, and the error returns of the WriteXRequest helpers.
// These tests pin each of them.

// With the test-override vars empty, each dir() accessor must return its
// exported production constant — that is the branch production actually runs.
func TestRequestDirProductionDefaults(t *testing.T) {
	oldPR, oldMerge, oldReview := prRequestDirForTest, mergeRequestDirForTest, reviewRequestDirForTest
	prRequestDirForTest, mergeRequestDirForTest, reviewRequestDirForTest = "", "", ""
	t.Cleanup(func() {
		prRequestDirForTest, mergeRequestDirForTest, reviewRequestDirForTest = oldPR, oldMerge, oldReview
	})

	if got := prRequestDir(); got != PRRequestDir {
		t.Errorf("prRequestDir() = %q, want %q", got, PRRequestDir)
	}
	if got := mergeRequestDir(); got != MergeRequestDir {
		t.Errorf("mergeRequestDir() = %q, want %q", got, MergeRequestDir)
	}
	if got := reviewRequestDir(); got != ReviewRequestDir {
		t.Errorf("reviewRequestDir() = %q, want %q", got, ReviewRequestDir)
	}
}

// A nil *Client (no GitHub creds) must make the single-pass entry points a
// no-op rather than a nil-pointer panic — the CLI/test callers rely on that.
func TestProcessRequestsOnceNilClientNoPanic(t *testing.T) {
	var c *Client
	ctx := context.Background()
	c.ProcessPRRequestsOnce(ctx)
	c.ProcessMergeRequestsOnce(ctx)
	c.ProcessReviewRequestsOnce(ctx)
}

// SetMergeReEngageHook must tolerate a nil receiver, install a hook on a real
// client, and restore quarantine-only behavior when handed nil.
func TestSetMergeReEngageHook(t *testing.T) {
	var nilC *Client
	nilC.SetMergeReEngageHook(func(string, int) bool { return true }) // must not panic

	c := &Client{}
	c.SetMergeReEngageHook(func(string, int) bool { return true })
	if c.mergeReEngage == nil {
		t.Fatal("hook not installed")
	}
	if !c.mergeReEngage("o/r", 1) {
		t.Fatal("installed hook not invoked")
	}
	c.SetMergeReEngageHook(nil)
	if c.mergeReEngage != nil {
		t.Fatal("nil hook did not clear the field")
	}
}

// When the request dir cannot be created (a path component is a regular
// file), each WriteXRequest helper must surface the MkdirAll error.
func TestWriteRequestHelpersMkdirFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(file, "sub")

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "b", Title: "t", Agent: "a"}); err == nil {
		t.Error("WritePRRequest: expected MkdirAll error, got nil")
	}
	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 1, Agent: "a"}); err == nil {
		t.Error("WriteMergeRequest: expected MkdirAll error, got nil")
	}
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 1, Agent: "a"}); err == nil {
		t.Error("WriteReviewRequest: expected MkdirAll error, got nil")
	}
}

// When the dir exists but is not writable, the WriteFile error must be
// returned (root bypasses permission checks, so skip there — same guard as
// TestPrepareRequestDirs in request_dirs_test.go).
func TestWriteRequestHelpersWriteFileFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission denial cannot be simulated")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "b", Title: "t", Agent: "a"}); err == nil {
		t.Error("WritePRRequest: expected WriteFile error, got nil")
	}
	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 1, Agent: "a"}); err == nil {
		t.Error("WriteMergeRequest: expected WriteFile error, got nil")
	}
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 1, Agent: "a"}); err == nil {
		t.Error("WriteReviewRequest: expected WriteFile error, got nil")
	}
}

// Success path: each helper names the file after the sanitized agent
// (disallowed runes are stripped) and the payload round-trips. An empty
// agent falls back to "agent".
func TestWriteRequestHelpersRoundTrip(t *testing.T) {
	dir := t.TempDir()

	prPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "b", Title: "t", Agent: "qa/1 bot"})
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(prPath); !strings.HasPrefix(base, "qa1bot-") || !strings.HasSuffix(base, ".json") {
		t.Errorf("WritePRRequest filename %q: want sanitized-agent prefix and .json suffix", base)
	}
	var pr PRRequest
	data, err := os.ReadFile(prPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Repo != "o/r" || pr.Head != "b" || pr.Title != "t" {
		t.Errorf("WritePRRequest round-trip mismatch: %+v", pr)
	}

	mergePath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(mergePath); !strings.HasPrefix(base, "agent-") {
		t.Errorf("WriteMergeRequest empty-agent filename %q: want %q fallback prefix", base, "agent-")
	}
	var mr MergeRequest
	if data, err = os.ReadFile(mergePath); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &mr); err != nil {
		t.Fatal(err)
	}
	if mr.Repo != "o/r" || mr.Number != 7 {
		t.Errorf("WriteMergeRequest round-trip mismatch: %+v", mr)
	}

	reviewPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: "comment", Body: "b", Agent: "rev"})
	if err != nil {
		t.Fatal(err)
	}
	var rr ReviewRequest
	if data, err = os.ReadFile(reviewPath); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.Repo != "o/r" || rr.Number != 9 || rr.Event != "comment" {
		t.Errorf("WriteReviewRequest round-trip mismatch: %+v", rr)
	}
}
