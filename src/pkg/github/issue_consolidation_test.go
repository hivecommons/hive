package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// openTwin is one open issue served by consolidationMockServer.
type openTwin struct {
	number      int
	title, body string
	author      string
}

// closedTwin is one closed issue served by consolidationMockServer, closed
// closedAgo before now with the given state_reason.
type closedTwin struct {
	number              int
	title, body, author string
	stateReason         string
	closedAgo           time.Duration
	// isPR marks the entry as a pull request in the issues list.
	isPR bool
}

// consolidationMock records what a create attempt did: issues created, and
// the issue number + body of every comment posted.
type consolidationMock struct {
	created        int
	commentedOn    []int
	commentBodies  []string
	commentsStatus int // non-zero, non-200: every comment POST fails with it
	closed         []closedTwin
	closedLists    int // closed-issue list requests served
	// closedSiblingStatus, when non-zero, fails the closed-sibling count
	// list (the only closed list filtered by creator) with that status while
	// the rejected-twin closed list keeps working.
	closedSiblingStatus int
	// listCommentsStatus, when non-zero, fails every comment list GET with
	// that status; otherwise the list serves the comments posted so far.
	listCommentsStatus int
	// edited counts comment PATCHes; an edit rewrites commentBodies in place.
	edited int
	// editStatus, when non-zero, fails every comment PATCH with that status.
	editStatus int
}

// consolidationMockServer serves open issues newest-first (the order the
// GitHub list returns with sort=created&direction=desc), m.closed as the
// closed list (empty by default: no rejected twin, no closed sibling), label
// ensures, comments, and creates.
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
		case r.Method == "PATCH" && strings.Contains(r.URL.Path, "/comments/"):
			if m.editStatus != 0 {
				w.WriteHeader(m.editStatus)
				return
			}
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			parts := strings.Split(r.URL.Path, "/")
			id, _ := strconv.Atoi(parts[len(parts)-1])
			if id >= 1 && id <= len(m.commentBodies) {
				m.commentBodies[id-1] = in.Body
			}
			m.edited++
			_, _ = io.WriteString(w, `{"id":`+strconv.Itoa(id)+`}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			if m.listCommentsStatus != 0 {
				w.WriteHeader(m.listCommentsStatus)
				return
			}
			parts := strings.Split(r.URL.Path, "/")
			n, _ := strconv.Atoi(parts[len(parts)-2])
			out := make([]map[string]any, 0, len(m.commentedOn))
			for i, on := range m.commentedOn {
				if on == n {
					out = append(out, map[string]any{"id": i + 1, "body": m.commentBodies[i], "user": map[string]any{"login": testBot}})
				}
			}
			b, _ := json.Marshal(out)
			_, _ = w.Write(b)
		case r.Method == "GET" && issueNumberPathRE.MatchString(r.URL.Path):
			parts := strings.Split(r.URL.Path, "/")
			_, _ = io.WriteString(w, `{"number":`+parts[len(parts)-1]+`,"state":"open","labels":[],"html_url":"https://github.example/o/r/issues/`+parts[len(parts)-1]+`"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			if r.URL.Query().Get("state") != "open" {
				m.closedLists++
				if m.closedSiblingStatus != 0 && r.URL.Query().Get("creator") != "" {
					w.WriteHeader(m.closedSiblingStatus)
					return
				}
				out := make([]map[string]any, 0, len(m.closed))
				for _, c := range m.closed {
					at := time.Now().Add(-c.closedAgo).UTC().Format(time.RFC3339)
					entry := map[string]any{
						"number": c.number, "title": c.title, "body": c.body, "state": "closed",
						"state_reason": c.stateReason, "closed_at": at, "updated_at": at,
						"html_url": "https://github.example/o/r/issues/" + strconv.Itoa(c.number),
						"user":     map[string]any{"login": c.author},
					}
					if c.isPR {
						entry["pull_request"] = map[string]any{"url": "https://github.example/o/r/pull/" + strconv.Itoa(c.number)}
					}
					out = append(out, entry)
				}
				b, _ := json.Marshal(out)
				_, _ = w.Write(b)
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

var issueNumberPathRE = regexp.MustCompile(`/issues/[0-9]+$`)

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
			name:  "human-filed issue with a different file set",
			open:  openTwin{number: 41, author: "maintainer", title: "x.go is confusing", body: "pkg/x.go:3"},
			title: "[scanner] bug spanning x.go and y.go", body: "pkg/x.go:3 and pkg/y.go:8",
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

// dakota #415 (human) vs #1648 (bot): the same firmware bug, once by file set
// and once by canonical subject. The bot must comment on the human's issue
// instead of filing a twin or silently skipping.
func TestIssueRequestWatcher_FoldsIntoHumanFiledIssue(t *testing.T) {
	cases := []struct {
		name        string
		open        openTwin
		title, body string
		want        []string
		notWant     []string
	}{
		{
			name:  "file set",
			open:  openTwin{number: 415, author: "human", title: "HP ZBook black screen", body: "see build/esp.sh:12"},
			title: "[scanner] ESP path length too long", body: "build/esp.sh:40 overflows",
			want: []string{hiveFindingFoldedMarker, "exactly the same files", "`build/esp.sh`"},
		},
		{
			name:  "canonical subject",
			open:  openTwin{number: 415, author: "human", title: "HP ZBook black screen on boot", body: "no files"},
			title: "[scanner] HP ZBook black screen on boot", body: "no files either",
			want:    []string{hiveFindingFoldedMarker, "same subject as this one"},
			notWant: []string{"exactly the same files", "()"},
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
			if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != 415 {
				t.Fatalf("created=%d comments=%v, want one comment on human issue #415", m.created, m.commentedOn)
			}
			if !res.OK || !res.Consolidated || res.Number != 415 {
				t.Errorf("result = %+v, want consolidated into #415", res)
			}
			for _, want := range tc.want {
				if !strings.Contains(m.commentBodies[0], want) {
					t.Errorf("comment lacks %q:\n%s", want, m.commentBodies[0])
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(m.commentBodies[0], notWant) {
					t.Errorf("comment must not contain %q:\n%s", notWant, m.commentBodies[0])
				}
			}
		})
	}
}

// humanTwinRefiles are two reworded re-files of one finding against human
// issue #415, by file set and by canonical subject. Scanner sweeps reword a
// finding each time, so byte-identical dedupe never catches them.
var humanTwinRefiles = []struct {
	name     string
	open     openTwin
	requests [2]IssueRequest
}{
	{
		name: "file set",
		open: openTwin{number: 415, author: "human", title: "HP ZBook black screen", body: "see build/esp.sh:12"},
		requests: [2]IssueRequest{
			{Repo: "o/r", Agent: "scanner", Title: "[scanner] ESP path length too long", Body: "build/esp.sh:40 overflows"},
			{Repo: "o/r", Agent: "scanner", Title: "[scanner] ESP path exceeds firmware limit", Body: "build/esp.sh:41 writes past the limit"},
		},
	},
	{
		name: "canonical subject",
		open: openTwin{number: 415, author: "human", title: "HP ZBook black screen on boot", body: "no files"},
		requests: [2]IssueRequest{
			{Repo: "o/r", Agent: "scanner", Title: "[scanner] HP ZBook black screen on boot", Body: "no files either"},
			{Repo: "o/r", Agent: "scanner", Title: "[scanner] HP ZBook black screen on boot", Body: "reworded: still no files"},
		},
	},
}

// #11681: a human's issue must not collect one comment per scanner sweep.
// The second reworded re-file sees the folded marker and reuses #415 silently.
func TestIssueRequestWatcher_HumanFiledIssueCommentedAtMostOnce(t *testing.T) {
	for _, tc := range humanTwinRefiles {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{}
			srv := consolidationMockServer(t, []openTwin{tc.open}, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot
			dir := withIssueDir(t)

			for i, req := range tc.requests {
				reqPath, res := fileRequest(t, c, dir, req)
				if !res.OK || !res.Consolidated || !res.AlreadyExisted || res.Number != 415 {
					t.Errorf("request %d: result = %+v, want consolidated into #415", i+1, res)
				}
				if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
					t.Errorf("request %d must be consumed", i+1)
				}
			}
			if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != 415 {
				t.Fatalf("created=%d comments=%v, want exactly one comment on human issue #415", m.created, m.commentedOn)
			}
		})
	}
}

// A failed comment lookup is not evidence the finding was recorded: comment
// anyway rather than drop it.
func TestIssueRequestWatcher_HumanFiledCommentLookupFailureStillComments(t *testing.T) {
	for _, tc := range humanTwinRefiles {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{listCommentsStatus: http.StatusForbidden}
			srv := consolidationMockServer(t, []openTwin{tc.open}, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			_, res := fileRequest(t, c, withIssueDir(t), tc.requests[0])
			if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != 415 {
				t.Fatalf("created=%d comments=%v, want one comment on #415 despite the failed lookup", m.created, m.commentedOn)
			}
			if !res.OK || !res.Consolidated || res.Number != 415 {
				t.Errorf("result = %+v, want consolidated into #415", res)
			}
		})
	}
}

// #9376 regression guard: a bot-filed file-set twin still records every
// reworded re-file as its own comment, marker or not.
func TestIssueRequestWatcher_BotFiledFileSetTwinCommentedPerRefile(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 41, author: testBot, title: "[scanner] a in x.go", body: "pkg/x.go:3"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot
	dir := withIssueDir(t)

	for i, req := range []IssueRequest{
		{Repo: "o/r", Agent: "scanner", Title: "[scanner] b in x.go", Body: "pkg/x.go:9"},
		{Repo: "o/r", Agent: "scanner", Title: "[scanner] c in x.go", Body: "pkg/x.go:20"},
	} {
		_, res := fileRequest(t, c, dir, req)
		if !res.OK || !res.Consolidated || res.Number != 41 {
			t.Errorf("request %d: result = %+v, want consolidated into #41", i+1, res)
		}
	}
	if m.created != 0 || len(m.commentedOn) != 2 || m.commentedOn[0] != 41 || m.commentedOn[1] != 41 {
		t.Fatalf("created=%d comments=%v, want two comments on bot issue #41", m.created, m.commentedOn)
	}
	if !strings.Contains(m.commentBodies[0], hiveFindingFoldedMarker) {
		t.Errorf("consolidated comment lacks the folded marker:\n%s", m.commentBodies[0])
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

// The public CreateIssue wrapper keeps plain create semantics: consolidation
// is opted into by the agent watcher, the review-backlog sink and the fleet
// report through createIssue.
func TestCreateIssue_DirectCallDoesNotConsolidate(t *testing.T) {
	m := &consolidationMock{}
	srv := consolidationMockServer(t, []openTwin{
		{number: 41, author: testBot, title: "[scanner] a in x.go", body: "pkg/x.go:3"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	res, err := c.CreateIssue(context.Background(), CreateIssueRequest{Repo: "o/r", Title: "[review] b in x.go", Body: "pkg/x.go:9", Origin: "governor"})
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

// The steady-state #11239 cadence: each variant's PR merges and closes its
// issue before the next variant is filed, so the open count alone never
// reaches the threshold. A sibling closed as completed within the window
// counts, and the finding folds into the oldest OPEN sibling.
func TestIssueRequestWatcher_RecentlyClosedSiblingCountsTowardFold(t *testing.T) {
	m := &consolidationMock{closed: []closedTwin{
		{number: 55, author: testBot, title: "[sec-check] entity bypass", body: "scripts/lib/gate.mjs and test/fixtures/entity.svg", stateReason: "completed", closedAgo: 3 * 24 * time.Hour},
	}}
	srv := consolidationMockServer(t, []openTwin{
		{number: 61, author: testBot, title: "[sec-check] CDATA bypass", body: "scripts/lib/gate.mjs and test/fixtures/cdata.svg"},
		{number: 58, author: testBot, title: "[sec-check] DOCTYPE bypass", body: "scripts/lib/gate.mjs and test/fixtures/doctype.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "sec-check",
		Title: "[sec-check] image-set() bypass",
		Body:  "scripts/lib/gate.mjs:88 misses image-set(); see test/fixtures/imageset.svg",
	})
	if m.created != 0 {
		t.Fatalf("created %d issues, want the variant folded into #58", m.created)
	}
	if len(m.commentedOn) != 1 || m.commentedOn[0] != 58 {
		t.Fatalf("comments posted on %v, want exactly one on the oldest open sibling #58", m.commentedOn)
	}
	for _, want := range []string{"<!-- hive-finding-folded -->", "`scripts/lib/gate.mjs`", "2 open agent-filed issues (plus 1 closed as completed in the last 14 days)"} {
		if !strings.Contains(m.commentBodies[0], want) {
			t.Errorf("folded comment lacks %q:\n%s", want, m.commentBodies[0])
		}
	}
	if !res.OK || !res.Consolidated || res.Number != 58 {
		t.Errorf("result = %+v, want ok/consolidated naming #58", res)
	}
}

// The closed list is paged by creator, but a merged PR by the bot and an
// issue by someone else can still appear in it; neither counts as a sibling.
func TestIssueRequestWatcher_ClosedSiblingCountIgnoresPRsAndOtherAuthors(t *testing.T) {
	m := &consolidationMock{closed: []closedTwin{
		{number: 55, author: testBot, title: "[sec-check] entity bypass", body: "scripts/lib/gate.mjs and test/fixtures/entity.svg", stateReason: "completed", closedAgo: 3 * 24 * time.Hour},
		{number: 54, author: testBot, title: "fix: entity bypass", body: "scripts/lib/gate.mjs", stateReason: "completed", closedAgo: 2 * 24 * time.Hour, isPR: true},
		{number: 53, author: "someone-else", title: "gate.mjs cleanup", body: "scripts/lib/gate.mjs", stateReason: "completed", closedAgo: 1 * 24 * time.Hour},
	}}
	srv := consolidationMockServer(t, []openTwin{
		{number: 61, author: testBot, title: "[sec-check] CDATA bypass", body: "scripts/lib/gate.mjs and test/fixtures/cdata.svg"},
		{number: 58, author: testBot, title: "[sec-check] DOCTYPE bypass", body: "scripts/lib/gate.mjs and test/fixtures/doctype.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "sec-check",
		Title: "[sec-check] image-set() bypass",
		Body:  "scripts/lib/gate.mjs:88 misses image-set(); see test/fixtures/imageset.svg",
	})
	if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != 58 {
		t.Fatalf("created=%d comments=%v, want the variant folded into #58", m.created, m.commentedOn)
	}
	if want := "(plus 1 closed as completed in the last 14 days)"; !strings.Contains(m.commentBodies[0], want) {
		t.Errorf("folded comment should count only the bot-filed issue, lacks %q:\n%s", want, m.commentBodies[0])
	}
	if !res.OK || !res.Consolidated || res.Number != 58 {
		t.Errorf("result = %+v, want ok/consolidated naming #58", res)
	}
}

// When the recently-closed sibling lookup fails, the fold decision is made on
// open issues only: below the threshold the finding files normally, the
// failure is not surfaced and the request is not left queued.
func TestIssueRequestWatcher_ClosedSiblingLookupFailureFoldsOnOpenOnly(t *testing.T) {
	m := &consolidationMock{
		closed:              []closedTwin{{number: 55, author: testBot, title: "[sec-check] x", body: "scripts/lib/gate.mjs and test/x.svg", stateReason: "completed", closedAgo: 24 * time.Hour}},
		closedSiblingStatus: http.StatusBadGateway,
	}
	srv := consolidationMockServer(t, []openTwin{
		{number: 61, author: testBot, title: "[sec-check] a", body: "scripts/lib/gate.mjs and test/a.svg"},
		{number: 58, author: testBot, title: "[sec-check] b", body: "scripts/lib/gate.mjs and test/b.svg"},
	}, m)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = testBot

	reqPath, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
		Repo: "o/r", Agent: "sec-check", Title: "[sec-check] new bypass", Body: "scripts/lib/gate.mjs and test/c.svg",
	})
	if m.created != 1 || len(m.commentedOn) != 0 {
		t.Fatalf("created=%d comments=%v, want a fresh issue (two open siblings alone are below the fold threshold)", m.created, m.commentedOn)
	}
	if !res.OK || res.Consolidated || res.Number != 99 {
		t.Errorf("result = %+v, want ok, not consolidated, #99", res)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed after a successful create, stat err = %v", err)
	}
}

// A failed fold comment — onto a human-filed twin or onto the oldest open
// same-component sibling — must neither drop the finding nor file the
// duplicate: the request stays queued for retry, as for a file-set twin.
func TestIssueRequestWatcher_FoldCommentFailureRetries(t *testing.T) {
	cases := []struct {
		name        string
		open        []openTwin
		title, body string
	}{
		{
			name:  "human-filed subject twin",
			open:  []openTwin{{number: 415, author: "human", title: "HP ZBook black screen on boot", body: "no files"}},
			title: "[scanner] HP ZBook black screen on boot", body: "no files either",
		},
		{
			name: "same-component stream",
			open: []openTwin{
				{number: 63, author: testBot, title: "[scanner] CDATA bypass", body: "build/esp.sh and test/fixtures/cdata.svg"},
				{number: 61, author: testBot, title: "[scanner] DOCTYPE bypass", body: "build/esp.sh and test/fixtures/doctype.svg"},
				{number: 58, author: testBot, title: "[scanner] CSS escape bypass", body: "build/esp.sh and test/fixtures/escape.svg"},
			},
			title: "[scanner] ESP path length too long", body: "build/esp.sh:40 overflows; see test/fixtures/long.svg",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{commentsStatus: http.StatusBadGateway}
			srv := consolidationMockServer(t, tc.open, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			reqPath, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
				Repo: "o/r", Agent: "scanner", Title: tc.title, Body: tc.body,
			})
			if m.created != 0 {
				t.Fatalf("created %d issues after a failed fold comment, want 0", m.created)
			}
			if res.OK || res.Consolidated {
				t.Errorf("result = %+v, want not ok", res)
			}
			if _, err := os.Stat(reqPath); err != nil {
				t.Errorf("request must stay queued for retry: %v", err)
			}
		})
	}
}

func TestPickFoldComponent(t *testing.T) {
	a, b := &gh.Issue{Number: gh.Ptr(1)}, &gh.Issue{Number: gh.Ptr(2)}
	cases := []struct {
		name         string
		open, closed map[string]int
		oldest       map[string]*gh.Issue
		wantTarget   *gh.Issue
		wantPath     string
		wantOpen     int
		wantClosed   int
	}{
		{
			name: "nothing reaches the threshold",
			open: map[string]int{"x.go": 1}, closed: map[string]int{"x.go": 1}, oldest: map[string]*gh.Issue{"x.go": a},
		},
		{
			name: "closed siblings top up the open count",
			open: map[string]int{"x.go": 1}, closed: map[string]int{"x.go": 2}, oldest: map[string]*gh.Issue{"x.go": a},
			wantTarget: a, wantPath: "x.go", wantOpen: 1, wantClosed: 2,
		},
		{
			name: "a path with no open sibling never wins, however many closed",
			open: map[string]int{"x.go": 0, "y.go": 3}, closed: map[string]int{"x.go": 9}, oldest: map[string]*gh.Issue{"x.go": a, "y.go": b},
			wantTarget: b, wantPath: "y.go", wantOpen: 3,
		},
		{
			name: "a path without an oldest open issue is skipped",
			open: map[string]int{"x.go": 5}, oldest: map[string]*gh.Issue{},
		},
		{
			name: "most-cited path wins",
			open: map[string]int{"x.go": 3, "y.go": 4}, oldest: map[string]*gh.Issue{"x.go": a, "y.go": b},
			wantTarget: b, wantPath: "y.go", wantOpen: 4,
		},
		{
			name: "ties go to the lexically smallest path",
			open: map[string]int{"y.go": 3, "x.go": 3}, oldest: map[string]*gh.Issue{"x.go": a, "y.go": b},
			wantTarget: a, wantPath: "x.go", wantOpen: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, path, openCount, closedCount := pickFoldComponent(tc.open, tc.closed, tc.oldest)
			if target != tc.wantTarget || path != tc.wantPath || openCount != tc.wantOpen || closedCount != tc.wantClosed {
				t.Errorf("got (%v, %q, %d, %d), want (%v, %q, %d, %d)", target, path, openCount, closedCount, tc.wantTarget, tc.wantPath, tc.wantOpen, tc.wantClosed)
			}
		})
	}
}

// Closed siblings that are not completed, or completed outside the window,
// do not count; with no open sibling at all nothing folds — a finding is
// never folded into a closed issue.
func TestIssueRequestWatcher_ClosedSiblingsThatDoNotCountFile(t *testing.T) {
	twoOpen := []openTwin{
		{number: 61, author: testBot, title: "[sec-check] a", body: "scripts/lib/gate.mjs and test/a.svg"},
		{number: 58, author: testBot, title: "[sec-check] b", body: "scripts/lib/gate.mjs and test/b.svg"},
	}
	cases := []struct {
		name   string
		open   []openTwin
		closed []closedTwin
	}{
		{
			name: "not_planned sibling belongs to the rejected-twin gate",
			open: twoOpen,
			closed: []closedTwin{
				{number: 55, author: testBot, title: "[sec-check] x", body: "scripts/lib/gate.mjs and test/x.svg", stateReason: "not_planned", closedAgo: 2 * 24 * time.Hour},
			},
		},
		{
			name: "completed sibling outside the window",
			open: twoOpen,
			closed: []closedTwin{
				{number: 50, author: testBot, title: "[sec-check] y", body: "scripts/lib/gate.mjs and test/y.svg", stateReason: "completed", closedAgo: 20 * 24 * time.Hour},
			},
		},
		{
			name: "all siblings closed, none open",
			closed: []closedTwin{
				{number: 57, author: testBot, title: "[sec-check] p", body: "scripts/lib/gate.mjs and test/p.svg", stateReason: "completed", closedAgo: 1 * 24 * time.Hour},
				{number: 56, author: testBot, title: "[sec-check] q", body: "scripts/lib/gate.mjs and test/q.svg", stateReason: "completed", closedAgo: 2 * 24 * time.Hour},
				{number: 55, author: testBot, title: "[sec-check] r", body: "scripts/lib/gate.mjs and test/r.svg", stateReason: "completed", closedAgo: 3 * 24 * time.Hour},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{closed: tc.closed}
			srv := consolidationMockServer(t, tc.open, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{
				Repo: "o/r", Agent: "sec-check", Title: "[sec-check] new bypass", Body: "scripts/lib/gate.mjs and test/c.svg",
			})
			if m.created != 1 || len(m.commentedOn) != 0 || res.Consolidated || res.RejectedDuplicate {
				t.Fatalf("created=%d comments=%v result=%+v, want a fresh issue", m.created, m.commentedOn, res)
			}
		})
	}
}

// A pair is enough when the primary path is shared and the subject matches:
// the endusers #1210/#1253 and #1267/#1276 shapes.
func TestIssueRequestWatcher_FoldsHighConfidencePair(t *testing.T) {
	cases := []struct {
		name         string
		openTitle    string
		openBody     string
		title, body  string
		wantFold     bool
		wantOnNumber int
	}{
		{
			name:      "unreachable guard remeasured",
			openTitle: "[scanner] Unreachable guard in src/lib/walk.ts (12 branches)",
			openBody:  "src/lib/walk.ts:40 has an unreachable guard",
			title:     "[sec-check] Unreachable guard remeasured in src/lib/walk.ts (14 branches)",
			body:      "src/lib/walk.ts:41 unreachable guard, remeasured",
			wantFold:  true, wantOnNumber: 1210,
		},
		{
			name:      "unguarded isDirectory at two sites",
			openTitle: "[scanner] Unguarded isDirectory() call in src/fs/scan.ts",
			openBody:  "src/fs/scan.ts:10 and src/fs/other.ts",
			title:     "[scanner]: Unguarded isDirectory() call in src/fs/scan.ts: second site",
			body:      "src/fs/scan.ts:99 and src/fs/extra.ts",
			wantFold:  true, wantOnNumber: 1210,
		},
		{
			// File sets differ (an identical set is already the #9376
			// file-set twin), so only the shared primary path links them.
			name:      "path-only overlap keeps the higher bar",
			openTitle: "[scanner] Missing timeout in src/lib/walk.ts",
			openBody:  "src/lib/walk.ts:5 and src/lib/timer.ts:9",
			title:     "[scanner] Unreachable guard in src/lib/walk.ts",
			body:      "src/lib/walk.ts:41 and src/lib/guard.ts:3",
		},
		{
			name:      "shared path is only secondary",
			openTitle: "[scanner] Unreachable guard in src/lib/a.ts",
			openBody:  "src/lib/a.ts and src/lib/walk.ts",
			title:     "[scanner] Unreachable guard in src/lib/b.ts",
			body:      "src/lib/b.ts and src/lib/walk.ts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &consolidationMock{}
			srv := consolidationMockServer(t, []openTwin{{number: 1210, author: testBot, title: tc.openTitle, body: tc.openBody}}, m)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.appBotLogin = testBot

			_, res := fileRequest(t, c, withIssueDir(t), IssueRequest{Repo: "o/r", Agent: "scanner", Title: tc.title, Body: tc.body})
			if tc.wantFold {
				if m.created != 0 || len(m.commentedOn) != 1 || m.commentedOn[0] != tc.wantOnNumber || !res.Consolidated {
					t.Fatalf("created=%d comments=%v result=%+v, want fold into #%d", m.created, m.commentedOn, res, tc.wantOnNumber)
				}
				return
			}
			if m.created != 1 || len(m.commentedOn) != 0 {
				t.Fatalf("created=%d comments=%v, want a fresh issue", m.created, m.commentedOn)
			}
		})
	}
}

// A second hive's App bot is a hive-filed author too; with no identity at all
// the checks stay off (and say so) instead of folding into a human's issue.
func TestIssueRequestWatcher_AdditionalBotLoginsAndEmptyIdentity(t *testing.T) {
	open := []openTwin{{number: 7, author: "other-hive[bot]", title: "[scanner] Unreachable guard in src/lib/walk.ts", body: "src/lib/walk.ts"}}
	req := IssueRequest{Repo: "o/r", Agent: "scanner", Title: "[scanner] Unreachable guard again in src/lib/walk.ts", Body: "src/lib/walk.ts"}

	t.Run("additional login folds", func(t *testing.T) {
		m := &consolidationMock{}
		srv := consolidationMockServer(t, open, m)
		defer srv.Close()
		c := issueTestClient(t, srv.URL)
		c.appBotLogin = testBot
		c.SetAdditionalBotLogins([]string{" Other-Hive[bot] "})
		_, res := fileRequest(t, c, withIssueDir(t), req)
		if m.created != 0 || !res.Consolidated || res.Number != 7 {
			t.Fatalf("created=%d result=%+v, want fold into #7", m.created, res)
		}
	})
	t.Run("empty identity files", func(t *testing.T) {
		m := &consolidationMock{}
		srv := consolidationMockServer(t, open, m)
		defer srv.Close()
		c := issueTestClient(t, srv.URL)
		c.appBotLogin = ""
		_, res := fileRequest(t, c, withIssueDir(t), req)
		if m.created != 1 || res.Consolidated {
			t.Fatalf("created=%d result=%+v, want a fresh issue", m.created, res)
		}
	})
}
