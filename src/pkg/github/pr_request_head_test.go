package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func head422Error() error {
	return fmt.Errorf("creating PR o/r HEAD->main: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
		Message:  "Validation Failed",
		Errors:   []gh.Error{{Resource: "PullRequest", Field: "head", Code: "invalid"}},
	})
}

func TestInvalidHeadReason(t *testing.T) {
	for _, head := range []string{"HEAD", " HEAD ", "head", "@", "", "refs/tags/v1", "-x", "a..b", "a b", "a/", "x.lock", "a~1", "a:b"} {
		if invalidHeadReason(head) == "" {
			t.Errorf("head %q must be refused", head)
		}
	}
	if r := invalidHeadReason("HEAD"); !strings.Contains(r, "detached-HEAD") {
		t.Errorf("detached-HEAD reason must say so, got %q", r)
	}
	for _, head := range []string{"hive/sec-check/20261117-220300", "scanner/fix-1", "refs/heads/docs/x", "feature/HEAD-fix", "v5"} {
		if r := invalidHeadReason(head); r != "" {
			t.Errorf("head %q must pass, got %q", head, r)
		}
	}
}

// Detached HEAD: the request is refused once, before any GitHub call, with a
// terminal .rejected quarantine and a result naming the cause.
func TestPRRequestWatcher_DetachedHeadRejectedOnceWithoutGitHubCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&logs, nil)))
	c.prAuthz = func(string, int) error { return nil }

	dir := t.TempDir()
	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "HEAD", Title: "fix: thing", Body: "fixes the thing", Agent: "sec-check"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.processPRRequestsInDir(context.Background(), func() time.Time { return now }, dir)

	if n := calls.Load(); n != 0 {
		t.Fatalf("GitHub was called %d times for a head that cannot be a branch", n)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("request not quarantined as .rejected: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("request must leave the queue")
	}
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	var res PRResponse
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "detached-HEAD") {
		t.Fatalf("result must report the detached HEAD: %+v", res)
	}
	if n := strings.Count(logs.String(), "level=ERROR msg=\"pr-request watcher: head is not a pushable branch, request rejected\""); n != 1 {
		t.Fatalf("want exactly one ERROR, got %d:\n%s", n, logs.String())
	}
	// A later tick finds nothing to retry.
	c.processPRRequestsInDir(context.Background(), func() time.Time { return now.Add(time.Hour) }, dir)
	if n := calls.Load(); n != 0 {
		t.Fatalf("rejected request was retried")
	}
}

func TestCreatePRRefusesDetachedHead(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	_, err := c.CreatePR(context.Background(), "o/r", "HEAD", "main", "t", "b")
	var bad *errInvalidHead
	if !errors.As(err, &bad) {
		t.Fatalf("want errInvalidHead, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("CreatePR touched GitHub for head HEAD")
	}
}

func TestReauthorBranchSignedSkipsNonBranchHead(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	got := c.reauthorBranchSigned(context.Background(), PRRequest{Repo: "o/r", Head: "HEAD"})
	if got.OID != "" || !strings.Contains(got.Skipped, "not a branch") || calls.Load() != 0 {
		t.Fatalf("signed path must skip a non-branch head without API calls: %+v calls=%d", got, calls.Load())
	}
}

// 422 head invalid: retried a bounded number of times (the same response also
// means "push not landed yet"), then terminal and recorded once at ERROR.
func TestFailPRRequestHead422IsBounded(t *testing.T) {
	var logs bytes.Buffer
	c := NewClientForTest("http://127.0.0.1:0", "o", nil, slog.New(slog.NewTextHandler(&logs, nil)))
	dir := t.TempDir()
	path := filepath.Join(dir, "sec-check-1.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := PRRequest{Repo: "o/r", Head: "hive/sec-check/1", Agent: "sec-check"}
	now := time.Now()
	for i := 1; i < prRequestHeadInvalidMaxAttempts; i++ {
		c.failPRRequest(path, req, head422Error(), func() time.Time { return now })
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("attempt %d: request must stay queued: %v", i, err)
		}
	}
	c.failPRRequest(path, req, head422Error(), func() time.Time { return now })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("request must leave the queue after %d attempts", prRequestHeadInvalidMaxAttempts)
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatalf("terminal 422 must quarantine as .rejected: %v", err)
	}
	if c.prRetries.attempts(path) != 0 {
		t.Fatalf("retry state must be cleared")
	}
	if n := strings.Count(logs.String(), "level=ERROR msg=\"pr-request watcher: head is not a pushable branch, request rejected\""); n != 1 {
		t.Fatalf("want one terminal ERROR, got %d:\n%s", n, logs.String())
	}
	b, _ := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if !strings.Contains(string(b), "422") {
		t.Fatalf("result must record the 422: %s", b)
	}
}

func TestFailPRRequestOther422StaysRetryable(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusUnprocessableEntity},
		Errors:   []gh.Error{{Resource: "PullRequest", Field: "base", Code: "invalid"}},
	}
	for i := 0; i < prRequestHeadInvalidMaxAttempts+2; i++ {
		c.failPRRequest(path, PRRequest{Repo: "o/r", Head: "feat"}, other, time.Now)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a non-head 422 must keep the ordinary retry policy: %v", err)
	}
}
