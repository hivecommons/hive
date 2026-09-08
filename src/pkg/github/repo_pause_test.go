package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// pauseAll returns a predicate matching the named repos, bare or qualified,
// the way config.IsRepoPaused does.
func pauseAll(repos ...string) func(string) bool {
	set := make(map[string]bool, len(repos))
	for _, r := range repos {
		set[strings.ToLower(r)] = true
	}
	return func(repo string) bool {
		if set[strings.ToLower(repo)] {
			return true
		}
		// Accept the "owner/name" spelling of a bare entry too.
		if i := strings.LastIndex(repo, "/"); i >= 0 {
			return set[strings.ToLower(repo[i+1:])]
		}
		return false
	}
}

// The work scope narrows; the repo inventory does not. primaryRepo() reads
// getRepos(), and filtering there would silently re-point the hive's primary
// repo at another repository the moment an operator paused the first one.
func TestActiveRepos_NarrowsScopeNotIdentity(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r", "second", "third"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := c.activeRepos(); len(got) != 3 {
		t.Fatalf("activeRepos() = %v with nothing paused, want all three", got)
	}

	c.SetRepoPausedFunc(pauseAll("r"))
	got := c.activeRepos()
	if len(got) != 2 || got[0] != "second" || got[1] != "third" {
		t.Fatalf("activeRepos() = %v, want the two unpaused repos in order", got)
	}
	if all := c.getRepos(); len(all) != 3 {
		t.Errorf("getRepos() = %v, want the full inventory", all)
	}
	if p := c.primaryRepo(); p != "r" {
		t.Errorf("primaryRepo() = %q, want %q — pausing a repo must not re-point the hive's primary", p, "r")
	}
	if !c.RepoIsPaused("r") || !c.RepoIsPaused("o/r") {
		t.Error("RepoIsPaused should match both the bare and the qualified spelling")
	}
	if c.RepoIsPaused("second") {
		t.Error("RepoIsPaused matched an unpaused repo")
	}
}

func TestRepoIsPaused_NilClientAndNilFunc(t *testing.T) {
	var nilClient *Client
	if nilClient.RepoIsPaused("r") {
		t.Error("nil client reported a pause")
	}
	c := NewClientForTest("http://127.0.0.1:1", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c.RepoIsPaused("r") {
		t.Error("a client with no predicate reported a pause")
	}
}

// The proxy hard-denies direct POST /pulls for every agent mode, so this relay
// is the ONLY way an agent opens a PR — and the hive's fulfilment does not
// traverse the proxy. Without a gate here, a paused repo still received agent
// PRs, which is the headline thing pause exists to stop.
func TestPRRequestWatcher_RefusesPausedRepo(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetRepoPausedFunc(pauseAll("r"))

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d PRs opened on a paused repo, want 0", created)
	}
	// Quarantined, not retried forever: a pause cannot be resolved by trying again.
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Errorf("request was not quarantined: %v", err)
	}
	resBytes, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(resBytes, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Error("result reports OK for a refused request")
	}
	if !strings.Contains(res.Error, "paused") || !strings.Contains(res.Error, "o/r") {
		t.Errorf("result does not say the repo is paused: %q", res.Error)
	}
}

func TestPRRequestWatcher_UnpausedRepoStillOpens(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetRepoPausedFunc(pauseAll("some-other-repo"))

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-2", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 1 {
		t.Errorf("pausing one repo blocked a PR on another: %d created, want 1", created)
	}
}

// Same reasoning for merges: PUT /pulls/{n}/merge is hard-denied at the proxy,
// so this watcher is the only agent-reachable merge path.
func TestMergeRequestWatcher_RefusesPausedRepo(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetRepoPausedFunc(pauseAll("r"))

	dir := t.TempDir()
	old := mergeRequestDirForTest
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = old }()

	// UpdateBranch true: a paused repo must receive no write at all, not even
	// the head-branch push that "update branch" performs before the merge.
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7, Agent: "quality", ExpectSHA: "deadbeef", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}

	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("%d merges on a paused repo, want 0", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("merge request was not quarantined: %v", err)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK {
		t.Error("result reports OK for a refused merge")
	}
	if !strings.Contains(resp.Error, "paused") {
		t.Errorf("result does not say the repo is paused: %q", resp.Error)
	}
}

// Issue, comment and claim requests use the hive's credentials, bypassing the
// proxy. A paused repo must receive no API calls from any of these paths.
func TestIssueRequestWatcher_RepoPause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		req      IssueRequest
		endpoint string
		response string
	}{
		{
			name:     "default issue",
			req:      IssueRequest{Title: "[scanner] finding", Body: "detail", Labels: []string{"agent/security"}},
			endpoint: "/repos/o/r/issues",
			response: `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`,
		},
		{
			name:     "explicit issue",
			req:      IssueRequest{Kind: "issue", Title: "[scanner] finding", Body: "detail", Labels: []string{"agent/security"}},
			endpoint: "/repos/o/r/issues",
			response: `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`,
		},
		{
			name:     "comment",
			req:      IssueRequest{Kind: "comment", Number: 42, Body: "triage note"},
			endpoint: "/repos/o/r/issues/42/comments",
			response: `{"id":1}`,
		},
		{
			name:     "claim",
			req:      IssueRequest{Kind: "claim", Number: 42},
			endpoint: "/repos/o/r/issues/42/labels",
			response: `[{"name":"hive/claimed-by-scanner"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
					_, _ = io.WriteString(w, `[]`)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
					_, _ = io.WriteString(w, `{"name":"agent/security"}`)
				case r.Method == http.MethodPost && r.URL.Path == tc.endpoint:
					writes.Add(1)
					_, _ = io.WriteString(w, tc.response)
				default:
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.SetRepoPausedFunc(pauseAll("r"))
			dir := withIssueDir(t)
			req := tc.req
			req.Repo = "o/r"
			req.Agent = "scanner"
			reqPath, err := WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}

			c.ProcessIssueRequestsOnce(context.Background())

			if got := calls.Load(); got != 0 {
				t.Errorf("paused repo received %d GitHub calls, want 0", got)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("denied request remains in the queue: %v", err)
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("request was not quarantined: %v", err)
			}
			resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
			b, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			var res IssueResponse
			if err := json.Unmarshal(b, &res); err != nil {
				t.Fatal(err)
			}
			if res.OK || !strings.Contains(res.Error, "paused") || !strings.Contains(res.Error, req.Repo) {
				t.Errorf("want a refusal naming the paused repo, got %+v", res)
			}

			// Resume this repo while another stays paused. Quarantined requests
			// must not replay; a fresh request must succeed on the same client.
			c.SetRepoPausedFunc(pauseAll("some-other-repo"))
			c.ProcessIssueRequestsOnce(context.Background())
			if got := calls.Load(); got != 0 {
				t.Errorf("quarantined request was replayed: %d GitHub calls, want 0", got)
			}
			reqPath, err = WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if got := writes.Load(); got != 1 {
				t.Errorf("unpaused repo received %d writes, want 1", got)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("successful request was not consumed: %v", err)
			}
			b, err = os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			var resumed IssueResponse
			if err := json.Unmarshal(b, &resumed); err != nil {
				t.Fatal(err)
			}
			if !resumed.OK {
				t.Errorf("fresh request on resumed repo failed: %+v", resumed)
			}
		})
	}
}

func TestReviewRequestWatcher_RepoPause(t *testing.T) {
	for _, event := range []string{"approve", "request_changes", "comment"} {
		t.Run(event, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/pulls/7/reviews" {
					t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":1}`)
			}))
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			c.SetRepoPausedFunc(pauseAll("r"))
			dir := withReviewDir(t)
			req := ReviewRequest{Repo: "o/r", Number: 7, Event: event, Body: "review feedback", Agent: "reviewer"}
			reqPath, err := WriteReviewRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}

			c.ProcessReviewRequestsOnce(context.Background())

			if got := calls.Load(); got != 0 {
				t.Errorf("paused repo received %d GitHub calls, want 0", got)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("denied request remains in the queue: %v", err)
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("request was not quarantined: %v", err)
			}
			b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			var res ReviewResponse
			if err := json.Unmarshal(b, &res); err != nil {
				t.Fatal(err)
			}
			if res.OK || !strings.Contains(res.Error, "paused") || !strings.Contains(res.Error, req.Repo) {
				t.Errorf("want a refusal naming the paused repo, got %+v", res)
			}

			c.SetRepoPausedFunc(pauseAll("some-other-repo"))
			c.ProcessReviewRequestsOnce(context.Background())
			if got := calls.Load(); got != 0 {
				t.Errorf("quarantined request was replayed: %d GitHub calls, want 0", got)
			}
			reqPath, err = WriteReviewRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessReviewRequestsOnce(context.Background())
			if got := calls.Load(); got != 1 {
				t.Errorf("unpaused repo received %d GitHub calls, want 1", got)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("successful request was not consumed: %v", err)
			}
			b, err = os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			var resumed ReviewResponse
			if err := json.Unmarshal(b, &resumed); err != nil {
				t.Fatal(err)
			}
			if !resumed.OK {
				t.Errorf("fresh request on resumed repo failed: %+v", resumed)
			}
		})
	}
}
