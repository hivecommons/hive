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
	"time"
)

// mock GitHub API for PR create + list. created counts POST /pulls calls.
func newPRMockServer(t *testing.T, existingHead string, created *int) *httptest.Server {
	t.Helper()
	return newPRMockServerLabels(t, existingHead, created, nil)
}

// newPRMockServerLabels extends newPRMockServer to also mock
// POST /issues/{n}/labels; when addedLabels != nil it records each labels-add
// payload (as the JSON array of label names) so a test can assert the "hold"
// label was (or was not) applied.
func newPRMockServerLabels(t *testing.T, existingHead string, created *int, addedLabels *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r"):
			// DefaultBranch: every request in this file omits Base, so CreatePR
			// resolves it here. "main" keeps existing assertions in this file
			// (which expect base "main") unchanged; the non-"main" regression
			// coverage for kubestellar/hive#4928 lives in pullrequest_base_test.go.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
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
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			body, _ := io.ReadAll(r.Body)
			var labels []string
			_ = json.Unmarshal(body, &labels)
			if addedLabels != nil {
				*addedLabels = append(*addedLabels, labels...)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
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

// F6: the hold label is applied server-side after a PR is opened iff the
// injected decider says the current ACMM level is hold-gated. This is the check
// the dead gh-wrapper.sh tail failed to perform (it sat after `exec
// hive-open-pr`), so hold-gated PRs were opened unlabeled.
func TestPRRequestWatcher_HoldLabelApplied(t *testing.T) {
	cases := []struct {
		name     string
		holdFn   func() bool
		wantHold bool
	}{
		{"hold-gated level applies hold", func() bool { return true }, true},
		{"non-hold-gated level applies nothing", func() bool { return false }, false},
		{"nil decider applies nothing", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := 0
			var added []string
			srv := newPRMockServerLabels(t, "", &created, &added)
			defer srv.Close()

			c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			c.prAuthz = func(agent string, uid int) error { return nil }
			c.prHoldLabel = tc.holdFn

			dir := t.TempDir()
			old := prRequestDirForTest
			prRequestDirForTest = dir
			defer func() { prRequestDirForTest = old }()

			_, _ = WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/hold", Title: "gated PR", Agent: "scanner"})
			c.ProcessPRRequestsOnce(context.Background())

			if created != 1 {
				t.Fatalf("expected the PR to be created once, got %d", created)
			}
			gotHold := false
			for _, l := range added {
				if l == "hold" {
					gotHold = true
				}
			}
			if gotHold != tc.wantHold {
				t.Errorf("hold label applied=%v, want %v (labels seen: %v)", gotHold, tc.wantHold, added)
			}
		})
	}
}

// newPRFailingMockServer fails the first `fails` POST /pulls calls with a 500,
// then behaves like newPRMockServer. The dedupe GET always returns empty.
func newPRFailingMockServer(t *testing.T, created *int, fails *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			if *fails > 0 {
				*fails--
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if created != nil {
				*created++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// Transient failure: the request survives with its error recorded, retries are
// suppressed inside the backoff window (NOT re-attempted every tick — the
// every-tick loop is what burned the org's secondary rate limit), and the
// request succeeds once the backoff elapses.
func TestPRRequestWatcher_RetriesWithBackoff(t *testing.T) {
	created, fails := 0, 1
	srv := newPRFailingMockServer(t, &created, &fails)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "[scanner] fix", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processPRRequests(context.Background(), clock)
	if created != 0 {
		t.Fatalf("first attempt should have failed")
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request must survive a transient failure: %v", err)
	}
	// Within backoff: skipped entirely.
	c.processPRRequests(context.Background(), clock)
	if created != 0 {
		t.Fatalf("retry must be suppressed inside the backoff window")
	}
	// After backoff: retried and succeeds; request consumed.
	now = now.Add(requestRetryBase + time.Second)
	c.processPRRequests(context.Background(), clock)
	if created != 1 {
		t.Fatalf("expected creation on post-backoff retry, got %d", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed after eventual success")
	}
}

// Give-up horizon: a request still failing past requestRetryMaxAge is
// quarantined as .failed and never retried again.
func TestPRRequestWatcher_QuarantinesAfterMaxAge(t *testing.T) {
	created, fails := 0, 1000000
	srv := newPRFailingMockServer(t, &created, &fails)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/never-lands", Title: "[scanner] never", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processPRRequests(context.Background(), clock) // first failure starts the horizon clock
	now = now.Add(requestRetryMaxAge + time.Hour)
	c.processPRRequests(context.Background(), clock) // exceeds horizon -> quarantine

	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("original request should be renamed away")
	}
	// And it is never attempted again even after more time passes.
	before := created
	now = now.Add(time.Hour)
	c.processPRRequests(context.Background(), clock)
	if created != before {
		t.Errorf("quarantined request must not be retried")
	}
}
