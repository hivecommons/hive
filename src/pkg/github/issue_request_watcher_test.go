package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newIssueMockServer mocks GET /issues (dedupe list) and POST /issues (create)
// plus label ensure endpoints. existingTitle, when non-empty, is returned by
// the list as an open issue titled exactly that. created counts POST /issues.
// failCreates, when >0, makes the first N create attempts return 502.
func newIssueMockServer(t *testing.T, existingTitle string, created *int, failCreates *int, commented *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			if commented != nil {
				*commented++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			w.Header().Set("Content-Type", "application/json")
			if existingTitle != "" {
				b, _ := json.Marshal([]map[string]any{{
					"number": 7, "title": existingTitle,
					"html_url": "https://github.example/o/r/issues/7",
				}})
				_, _ = w.Write(b)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			if failCreates != nil && *failCreates > 0 {
				*failCreates--
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if created != nil {
				*created++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func issueTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := testClient(t, srvURL)
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	return c
}

func withIssueDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := issueRequestDirForTest
	issueRequestDirForTest = dir
	t.Cleanup(func() { issueRequestDirForTest = old })
	return dir
}

// End-to-end: a request file becomes an issue, is consumed, result written.
func TestIssueRequestWatcher_CreatesAndConsumes(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "", &created, nil, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[sec-check] High: CVE-2026-1", Body: "detail",
		Labels: []string{"agent/security"}, Agent: "sec-check",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessIssueRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("expected 1 issue created, got %d", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request file should be removed after success")
	}
	var res IssueResponse
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Number != 99 || res.AlreadyExisted {
		t.Errorf("unexpected result: %+v", res)
	}
}

// Idempotency: an open issue with the identical title is reused, not duplicated.
// This is the guard against the "create timed out but actually landed" ambiguity
// and against watcher crash between create and request-file removal.
func TestIssueRequestWatcher_DedupesByTitle(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "[sec-check] already filed", &created, nil, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[sec-check] already filed", Body: "detail", Agent: "sec-check",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessIssueRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("expected 0 issues created (dedupe), got %d", created)
	}
	var res IssueResponse
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	_ = json.Unmarshal(b, &res)
	if !res.OK || !res.AlreadyExisted || res.Number != 7 {
		t.Errorf("expected reuse of issue 7, got %+v", res)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed on reuse")
	}
}

// Transient failure: request survives, error recorded, retried after backoff,
// then succeeds.
func TestIssueRequestWatcher_RetriesTransientFailure(t *testing.T) {
	created := 0
	fails := 1
	srv := newIssueMockServer(t, "", &created, &fails, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] flaky forge", Body: "x", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processIssueRequests(context.Background(), clock)
	if created != 0 {
		t.Fatalf("first attempt should have failed")
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request must survive a transient failure: %v", err)
	}
	// Within backoff: skipped.
	c.processIssueRequests(context.Background(), clock)
	if created != 0 {
		t.Fatalf("retry should be suppressed inside the backoff window")
	}
	// After backoff: retried and succeeds.
	now = now.Add(issueRetryBase + time.Second)
	c.processIssueRequests(context.Background(), clock)
	if created != 1 {
		t.Fatalf("expected creation on post-backoff retry, got %d", created)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed after eventual success")
	}
}

// Give-up horizon: a request failing past issueRequestMaxAge is quarantined
// as .failed and never retried again.
func TestIssueRequestWatcher_QuarantinesAfterMaxAge(t *testing.T) {
	created := 0
	fails := 1000
	srv := newIssueMockServer(t, "", &created, &fails, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Repo: "o/r", Title: "[scanner] never lands", Body: "x", Agent: "scanner",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processIssueRequests(context.Background(), clock) // first failure, starts the clock
	now = now.Add(issueRequestMaxAge + time.Hour)
	c.processIssueRequests(context.Background(), clock) // exceeds horizon → quarantine

	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("original request should be renamed away")
	}
}

// Malformed requests (bad JSON, missing fields, unknown kind) are quarantined
// as .bad immediately — they can never succeed, so they must never retry.
func TestIssueRequestWatcher_QuarantinesMalformed(t *testing.T) {
	srv := newIssueMockServer(t, "", nil, nil, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	badJSON := dir + "/agent-1.json"
	_ = os.WriteFile(badJSON, []byte("{not json"), 0o644)
	noTitle, _ := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Agent: "a"})
	badKind, _ := WriteIssueRequest(dir, IssueRequest{Kind: "wat", Repo: "o/r", Title: "t", Agent: "a"})
	noNumber, _ := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Body: "b", Agent: "a"})

	c.ProcessIssueRequestsOnce(context.Background())

	for _, p := range []string{badJSON, noTitle, badKind, noNumber} {
		if _, err := os.Stat(p + ".bad"); err != nil {
			t.Errorf("expected %s.bad quarantine: %v", p, err)
		}
	}
}

// Policy: a nil authorizer fails closed; a denying authorizer quarantines
// as .denied. Neither creates anything.
func TestIssueRequestWatcher_AuthzFailsClosed(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "", &created, nil, nil)
	defer srv.Close()
	dir := withIssueDir(t)

	// nil authorizer
	c := testClient(t, srv.URL)
	c.issueAuthz = nil
	p1, _ := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "t", Agent: "a"})
	c.ProcessIssueRequestsOnce(context.Background())
	if _, err := os.Stat(p1 + ".denied"); err != nil {
		t.Fatalf("nil authorizer must deny: %v", err)
	}

	// explicit denial (e.g. advisory-mode agent)
	c2 := testClient(t, srv.URL)
	c2.issueAuthz = func(agent string, uid int, kind string) error {
		return errors.New("agent is advisory-only")
	}
	p2, _ := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "t", Agent: "a"})
	c2.ProcessIssueRequestsOnce(context.Background())
	if _, err := os.Stat(p2 + ".denied"); err != nil {
		t.Fatalf("denial must quarantine: %v", err)
	}
	if created != 0 {
		t.Fatalf("nothing should be created under denial, got %d", created)
	}
}

// Comments: kind=comment posts to the comments endpoint and is consumed.
func TestIssueRequestWatcher_PostsComment(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 41, Body: "triage note", Agent: "quality",
	})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessIssueRequestsOnce(context.Background())

	if commented != 1 {
		t.Fatalf("expected 1 comment, got %d", commented)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("comment request should be consumed")
	}
}

// The authorizer receives the kind, so issue-vs-comment policy can differ.
func TestIssueRequestWatcher_AuthzSeesKind(t *testing.T) {
	srv := newIssueMockServer(t, "", nil, nil, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	var seen []string
	c.issueAuthz = func(agent string, uid int, kind string) error {
		seen = append(seen, kind)
		return errors.New("deny to stop processing")
	}
	dir := withIssueDir(t)
	_, _ = WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "t", Agent: "a"})
	_, _ = WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 3, Body: "b", Agent: "a"})

	c.ProcessIssueRequestsOnce(context.Background())

	got := strings.Join(seen, ",")
	if !strings.Contains(got, "issue") || !strings.Contains(got, "comment") {
		t.Fatalf("authorizer should see both kinds, saw %q", got)
	}
}
