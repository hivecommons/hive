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

// mock GitHub API for PR create + list. created counts POST /pulls calls.
func newPRMockServer(t *testing.T, existingHead string, created *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			// dedupe list — return one PR only when the requested head matches.
			head := r.URL.Query().Get("head") // "owner:branch"
			if existingHead != "" && strings.HasSuffix(head, ":"+existingHead) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `[{"number":11,"html_url":"https://github.com/o/r/pull/11","head":{"ref":"`+existingHead+`"}}]`)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			if created != nil {
				*created++
			}
			body, _ := io.ReadAll(r.Body)
			var np map[string]any
			_ = json.Unmarshal(body, &np)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42","title":"`+
				strings.ReplaceAll(asString(np["title"]), `"`, `\"`)+`"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// testClient returns a client whose PR authorizer ALLOWS everything, so the
// watcher's open/dedupe/quarantine paths can be exercised. Authorization itself
// is tested separately in TestPRRequestWatcher_Authz*.
func testClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := NewClientForTest(srvURL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.prAuthz = func(agent string, uid int) error { return nil }
	return c
}

// End-to-end: a request file is opened into a PR, consumed, and a result written.
func TestPRRequestWatcher_OpensAndConsumes(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
	// request consumed
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request file should be removed after success")
	}
	// result written with the PR number
	resBytes, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(resBytes, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Number != 42 || res.AlreadyExisted {
		t.Errorf("unexpected result: %+v", res)
	}
}

// Dedupe: an existing open PR for the head is reused, no second PR created.
func TestPRRequestWatcher_DedupesExistingPR(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "scanner/dup", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	_, _ = WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/dup", Title: "dup"})
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Errorf("existing PR should be reused, not re-created; got %d creates", created)
	}
}

// A malformed request is quarantined (renamed .bad), not retried forever.
func TestPRRequestWatcher_QuarantinesBadJSON(t *testing.T) {
	srv := newPRMockServer(t, "", nil)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	bad := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("bad request should be renamed away")
	}
	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Errorf("bad request should be quarantined as .bad: %v", err)
	}
}

// A denying authorizer (e.g. advisory agent / UID mismatch) must NOT open a PR
// and must quarantine the request as .denied (not retry it forever).
func TestPRRequestWatcher_AuthzDenyQuarantines(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.prAuthz = func(agent string, uid int) error { return context.DeadlineExceeded } // stand-in denial

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, _ := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "brainstorm/x", Title: "advisory tries to PR", Agent: "brainstorm"})
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Errorf("denied request must NOT create a PR; got %d", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("denied request should be renamed away")
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("denied request should be quarantined as .denied: %v", err)
	}
}

// A nil authorizer fails closed: no PR opened.
func TestPRRequestWatcher_NilAuthzFailsClosed(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// prAuthz deliberately left nil.

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	_, _ = WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/y", Title: "no authorizer", Agent: "scanner"})
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Errorf("nil authorizer must fail closed (no PR); got %d", created)
	}
}
