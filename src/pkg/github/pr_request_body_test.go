package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the lost-body guards added after quality/sec-check PRs went
// out whose entire body was the attribution footer: hive-open-pr silently
// dropped `--body-file`, the request carried body:"", and the watcher opened
// the PR anyway (e.g. Danathar/atomic-image-builder#223 — the agent wrote a
// full body beginning "Closes #222" and none of it reached GitHub). The
// watcher is the choke point every PR-open path funnels through, so the guard
// lives here as well as in the script.

func writeBodyTestRequest(t *testing.T, req PRRequest) (string, *Client, func()) {
	t.Helper()
	created := 0
	srv := newPRMockServer(t, "", &created)
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	cleanup := func() {
		prRequestDirForTest = old
		srv.Close()
	}
	reqPath, err := WritePRRequest(dir, req)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	return reqPath, c, cleanup
}

func readResult(t *testing.T, reqPath string) PRResponse {
	t.Helper()
	data, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res PRResponse
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// An empty (or whitespace-only) body is a permanent policy rejection: the PR
// is not opened, the request is quarantined as .rejected rather than retried,
// and the result explains that the body was lost, not merely "invalid".
func TestPRRequestWatcher_RejectsEmptyBody(t *testing.T) {
	for _, body := range []string{"", "  \n\t\n"} {
		reqPath, c, cleanup := writeBodyTestRequest(t, PRRequest{
			Repo: "o/r", Head: "quality/fix-1", Title: "[quality] fix", Body: body, Agent: "quality",
		})
		c.ProcessPRRequestsOnce(context.Background())

		if _, err := os.Stat(reqPath + ".rejected"); err != nil {
			t.Errorf("body %q: request should be quarantined as .rejected: %v", body, err)
		}
		res := readResult(t, reqPath)
		if res.OK || res.Number != 0 {
			t.Errorf("body %q: a body-less PR must not be opened: %+v", body, res)
		}
		if !strings.Contains(res.Error, "body is empty") {
			t.Errorf("body %q: rejection must say the body is empty, got %q", body, res.Error)
		}
		cleanup()
	}
}

// A request that declares its originating issue (hive-open-pr --issues) but
// whose body never references it is the same lost-content failure in partial
// form — rejected, with the missing issue named so the agent can fix the body.
func TestPRRequestWatcher_RejectsBodyMissingDeclaredIssue(t *testing.T) {
	reqPath, c, cleanup := writeBodyTestRequest(t, PRRequest{
		Repo: "o/r", Head: "quality/fix-7", Title: "[quality] fix",
		Body: "adds tests for the frobnicator", Agent: "quality", IssueN: []int{7},
	})
	defer cleanup()
	c.ProcessPRRequestsOnce(context.Background())

	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Fatalf("request should be quarantined as .rejected: %v", err)
	}
	res := readResult(t, reqPath)
	if res.OK {
		t.Fatalf("PR must not open when the body lost its issue reference: %+v", res)
	}
	if !strings.Contains(res.Error, "#7") {
		t.Errorf("rejection must name the missing issue, got %q", res.Error)
	}
}

// The declared-issue check accepts both a closing keyword and an explicit
// non-closing reference: "Closes #N" is the normal case, and "Refs #N" with a
// stated reason is the sanctioned exception — neither may be rejected.
func TestPRRequestWatcher_AcceptsClosesAndRefsForDeclaredIssue(t *testing.T) {
	for _, body := range []string{
		"## Related Issue\nCloses #1",
		"## Related Issue\nRefs #1 — the docs half stays open until the guide lands",
	} {
		reqPath, c, cleanup := writeBodyTestRequest(t, PRRequest{
			Repo: "o/r", Head: "quality/fix-1", Title: "[quality] fix",
			Body: body, Agent: "quality", IssueN: []int{1},
		})
		c.ProcessPRRequestsOnce(context.Background())

		res := readResult(t, reqPath)
		if !res.OK || res.Number != 42 {
			t.Errorf("body %q: expected the PR to open, got %+v", body, res)
		}
		cleanup()
	}
}

// Bare-repo requests ("r", not "o/r") must resolve the same default repo for
// the declared-issue check as the rest of the watcher, and a cross-repo
// reference (other/repo#7) must not satisfy a declaration for THIS repo.
func TestValidatePRRequestBody_RepoDefaulting(t *testing.T) {
	srv := newPRMockServer(t, "", nil)
	defer srv.Close()
	c := testClient(t, srv.URL)

	if reason := c.validatePRRequestBody(PRRequest{
		Repo: "r", Title: "t", Body: "Closes #7", IssueN: []int{7},
	}); reason != "" {
		t.Errorf("bare-repo request with matching reference rejected: %q", reason)
	}
	if reason := c.validatePRRequestBody(PRRequest{
		Repo: "o/r", Title: "t", Body: "Closes other/repo#7", IssueN: []int{7},
	}); reason == "" {
		t.Error("a cross-repo reference must not satisfy a same-repo issue declaration")
	}
}

// End-to-end through the real pieces: the actual bin/hive-open-pr.sh writes
// the request from a --body-file, and the actual watcher opens it against a
// mock GitHub — asserting the "Closes #N" line the agent wrote is present in
// the body GitHub receives. This is the full path that failed in production
// (script drops --body-file → watcher opens footer-only PR), pinned green.
func TestHiveOpenPRScript_ClosesLineSurvivesIntoOpenedPR(t *testing.T) {
	scriptPath, reqDir, root := stageHiveOpenPRScript(t)

	bodyPath := filepath.Join(root, "pr-body.md")
	const bodyText = "## Test Improvement\n\nadds the missing tests\n\n## Related Issue\nCloses #1\n"
	if err := os.WriteFile(bodyPath, []byte(bodyText), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", scriptPath,
		"--repo", "o/r", "--head", "quality/fix-1",
		"--title", "[quality] fix the thing",
		"--body-file", bodyPath, "--issues", "1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HIVE_AGENT=quality")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hive-open-pr.sh: %v\n%s", err, out)
	}

	var postedBody string
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r"):
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/compare/"):
			_, _ = io.WriteString(w, `{"files":[]}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/"):
			_, _ = io.WriteString(w, `{"number":1,"title":"ordinary issue","body":"implement the requested change","state":"open"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			created++
			raw, _ := io.ReadAll(r.Body)
			var np map[string]any
			_ = json.Unmarshal(raw, &np)
			postedBody = asString(np["body"])
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	old := prRequestDirForTest
	prRequestDirForTest = reqDir
	defer func() { prRequestDirForTest = old }()
	c.ProcessPRRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("expected the PR to be created once, got %d", created)
	}
	if !strings.Contains(postedBody, "Closes #1") {
		t.Fatalf("the Closes line the agent wrote did not reach GitHub; posted body:\n%s", postedBody)
	}
	if !strings.Contains(postedBody, "adds the missing tests") {
		t.Fatalf("the body content did not reach GitHub; posted body:\n%s", postedBody)
	}
}
