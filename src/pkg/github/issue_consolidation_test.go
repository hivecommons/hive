package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// openTwin is one open issue served by consolidationMockServer.
type openTwin struct {
	number      int
	title, body string
	author      string
}

// consolidationMock records what a create attempt did: issues created, and
// the issue number + body of every comment posted.
type consolidationMock struct {
	created        int
	commentedOn    []int
	commentBodies  []string
	commentsStatus int // non-zero, non-200: every comment POST fails with it
}

// consolidationMockServer serves open issues newest-first (the order the
// GitHub list returns with sort=created&direction=desc), an empty closed list
// (no rejected twin), label ensures, comments, and creates.
func consolidationMockServer(t *testing.T, open []openTwin, m *consolidationMock) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			if m.commentsStatus != 0 && m.commentsStatus != http.StatusOK {
				w.WriteHeader(m.commentsStatus)
				return
			}
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			parts := strings.Split(r.URL.Path, "/")
			n, _ := strconv.Atoi(parts[len(parts)-2])
			m.commentedOn = append(m.commentedOn, n)
			m.commentBodies = append(m.commentBodies, in.Body)
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			if r.URL.Query().Get("state") != "open" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			out := make([]map[string]any, 0, len(open))
			for _, o := range open {
				out = append(out, map[string]any{
					"number": o.number, "title": o.title, "body": o.body, "state": "open",
					"html_url": "https://github.example/o/r/issues/" + strconv.Itoa(o.number),
					"user":     map[string]any{"login": o.author},
				})
			}
			b, _ := json.Marshal(out)
			_, _ = w.Write(b)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues"):
			m.created++
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

const testBot = "hive-app[bot]"

// fileRequest drops one agent issue request, runs the watcher once, and
// returns the request path plus the parsed result.
func fileRequest(t *testing.T, c *Client, dir string, req IssueRequest) (string, IssueResponse) {
	t.Helper()
	reqPath, err := WriteIssueRequest(dir, req)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	var res IssueResponse
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return reqPath, res
}

// The #9376 failure: two scanner findings about the same test file were filed
// as two issues, worked as two PRs, and conflicted. The second finding must
// land on the open issue as a comment, not as a new issue.
func TestIssueRequestWatcher_ConsolidatesIntoOpenFileSetTwin(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{{
		number: 41, author: testBot,
		title: "[scanner] flaky retry assertion in client_test.go",
		body:  "TestRetry at pkg/foo/client_test.go:120 races the ticker",
	}}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	reqPath, res := fileRequest(t, c, dir, IssueRequest{
		Repo: "o/r", Agent: "scanner",
		Title: "[scanner] missing cleanup leaks goroutines in client_test.go",
		Body:  "TestDial at pkg/foo/client_test.go:310 never cancels its context",
	})

	if m.created != 0 {
		t.Fatalf("created %d issues, want 0 — the finding belongs on open issue #41", m.created)
	}
	if len(m.commentedOn) != 1 || m.commentedOn[0] != 41 {
		t.Fatalf("comments posted on %v, want exactly one on #41", m.commentedOn)
	}
	for _, want := range []string{"missing cleanup leaks goroutines", "client_test.go:310 never cancels", "`pkg/foo/client_test.go`"} {
		if !strings.Contains(m.commentBodies[0], want) {
			t.Errorf("consolidated comment lacks %q:\n%s", want, m.commentBodies[0])
		}
	}
	if !res.OK || !res.Consolidated || !res.AlreadyExisted || res.Number != 41 {
		t.Errorf("result = %+v, want ok/consolidated/already_existed naming #41", res)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("consolidated request must be consumed")
	}
}

// Several open twins: fold into the OLDEST, the one already carrying the work.
func TestIssueRequestWatcher_ConsolidatesIntoOldestTwin(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 52, author: testBot, title: "[scanner] b in x.go", body: "pkg/x.go:9"},
		{number: 40, author: testBot, title: "[scanner] a in x.go", body: "pkg/x.go:3"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "scanner", Title: "[scanner] c in x.go", Body: "pkg/x.go:20",
	})
	if res.Number != 40 || len(m.commentedOn) != 1 || m.commentedOn[0] != 40 {
		t.Fatalf("folded into %v (result %+v), want oldest #40", m.commentedOn, res)
	}
}

// Consolidation is exact file-set equality on App-bot issues only. Anything
// else — a different file set, a human's issue, no file references at all —
// is not positive evidence of shared work and must file normally.
func TestIssueRequestWatcher_FilesWhenNoOpenTwin(t *testing.T) {
	cases := []struct {
		name  string
		open  openTwin
		title string
		body  string
	}{
		{
			name:  "superset file set",
			open:  openTwin{number: 41, author: testBot, title: "[scanner] bug in x.go", body: "pkg/x.go:3"},
			title: "[scanner] bug spanning x.go and y.go", body: "pkg/x.go:3 and pkg/y.go:8",
		},
		{
			name:  "human-filed issue",
			open:  openTwin{number: 41, author: "maintainer", title: "x.go is confusing", body: "pkg/x.go:3"},
			title: "[scanner] bug in x.go", body: "pkg/x.go:3",
		},
		{
			name:  "no file references",
			open:  openTwin{number: 41, author: testBot, title: "[scanner] governance gap", body: "nothing"},
			title: "[scanner] review process undocumented", body: "no files named",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{}
			srv := consolidationMockServer(t, []openTwin{tc.open}, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
				Repo: "o/r", Agent: "scanner", Title: tc.title, Body: tc.body,
			})
			if m.created != 1 || len(m.commentedOn) != 0 || res.Consolidated || res.Number != 99 {
				t.Fatalf("created=%d comments=%v result=%+v, want a fresh issue #99", m.created, m.commentedOn, res)
			}
		})
	}
}

// A failed consolidation comment must neither drop the finding nor fall back
// to filing the duplicate: the request stays queued for retry.
func TestIssueRequestWatcher_ConsolidationCommentFailureRetries(t *testing.T) {
	m := &consolidationMock{commentsStatus: http.StatusBadGateway}
	srv := consolidationMockServer(t, []openTwin{
		{number: 41, author: testBot, title: "[scanner] a in x.go", body: "pkg/x.go:3"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	reqPath, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "scanner", Title: "[scanner] b in x.go", Body: "pkg/x.go:9",
	})
	if m.created != 0 {
		t.Fatalf("created %d issues after a failed consolidation, want 0", m.created)
	}
	if res.OK {
		t.Errorf("result = %+v, want not ok", res)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Errorf("request must stay queued for retry: %v", err)
	}
}

// Hive-internal CreateIssue callers (fleet report, review backlog) keep plain
// create semantics: consolidation is scoped to agent requests.
func TestCreateIssue_DirectCallDoesNotConsolidate(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 41, author: testBot, title: "[scanner] a in x.go", body: "pkg/x.go:3"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	res, err := c.CreateIssue(context.Background(), "o/r", "[review] b in x.go", "pkg/x.go:9", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.created != 1 || len(m.commentedOn) != 0 || res.Consolidated {
		t.Fatalf("created=%d comments=%v result=%+v, want a plain create", m.created, m.commentedOn, res)
	}
}

// The #11239 failure: a stream of variants against one file, each citing the
// shared file plus its own fixture, so no two file sets are equal. Once
// sameComponentFoldThreshold open App-bot issues cite the shared path, the
// next variant folds into the oldest instead of becoming another issue.
func TestIssueRequestWatcher_FoldsSameComponentStream(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 63, author: testBot, title: "[sec-check] CDATA bypass", body: "scripts/lib/svg-active-content.mjs and test/fixtures/cdata.svg"},
		{number: 61, author: testBot, title: "[sec-check] DOCTYPE bypass", body: "scripts/lib/svg-active-content.mjs and test/fixtures/doctype.svg"},
		{number: 60, author: "maintainer", title: "svg gate rewrite", body: "scripts/lib/svg-active-content.mjs"},
		{number: 58, author: testBot, title: "[sec-check] CSS escape bypass", body: "scripts/lib/svg-active-content.mjs and test/fixtures/escape.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "sec-check",
		Title: "[sec-check] image-set() bypass",
		Body:  "scripts/lib/svg-active-content.mjs:88 misses image-set(); see test/fixtures/imageset.svg",
	})
	if m.created != 0 {
		t.Fatalf("created %d issues, want the variant folded into #58", m.created)
	}
	if len(m.commentedOn) != 1 || m.commentedOn[0] != 58 {
		t.Fatalf("comments posted on %v, want exactly one on the oldest bot issue #58", m.commentedOn)
	}
	for _, want := range []string{"<!-- hive-finding-folded -->", "`scripts/lib/svg-active-content.mjs`", "3 open agent-filed issues", "image-set() bypass"} {
		if !strings.Contains(m.commentBodies[0], want) {
			t.Errorf("folded comment lacks %q:\n%s", want, m.commentBodies[0])
		}
	}
	if !res.OK || !res.Consolidated || !res.AlreadyExisted || res.Number != 58 {
		t.Errorf("result = %+v, want ok/consolidated naming #58", res)
	}
}

// Below the threshold, or when the only shared path is ubiquitous, an
// overlapping finding is ordinary and files normally.
func TestIssueRequestWatcher_SameComponentBelowThresholdFiles(t *testing.T) {
	cases := []struct {
		name string
		open []openTwin
		body string
	}{
		{
			name: "two open siblings",
			open: []openTwin{
				{number: 61, author: testBot, title: "[sec-check] a", body: "scripts/lib/gate.mjs and test/a.svg"},
				{number: 58, author: testBot, title: "[sec-check] b", body: "scripts/lib/gate.mjs and test/b.svg"},
			},
			body: "scripts/lib/gate.mjs and test/c.svg",
		},
		{
			name: "only ubiquitous path shared",
			open: []openTwin{
				{number: 63, author: testBot, title: "[scanner] a", body: "go.mod and pkg/a.go"},
				{number: 61, author: testBot, title: "[scanner] b", body: "go.mod and pkg/b.go"},
				{number: 58, author: testBot, title: "[scanner] c", body: "go.mod and pkg/c.go"},
			},
			body: "go.mod and pkg/d.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{}
			srv := consolidationMockServer(t, tc.open, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
				Repo: "o/r", Agent: "scanner", Title: "[scanner] new finding", Body: tc.body,
			})
			if m.created != 1 || len(m.commentedOn) != 0 || res.Consolidated {
				t.Fatalf("created=%d comments=%v result=%+v, want a fresh issue", m.created, m.commentedOn, res)
			}
		})
	}
}
