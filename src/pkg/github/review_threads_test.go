package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// gqlThread is a mock server's notion of one review thread. The mock renders
// it into both the per-PR reviewThreads query and the node(id:) re-fetch.
type gqlThread struct {
	ID         string
	Resolved   bool
	Outdated   bool
	Path       string
	Line       int
	Comments   []gqlComment
	PRNumber   int
	RepoOwner  string
	RepoName   string
	HeadRef    string
	DatabaseID int64
	// PRAuthor is the login GitHub shows as the PR's author. Rendered in the
	// node re-fetch so a fixture can state that a PR is human-authored (a
	// relay PR, #7638); the guard must not read it — the thread's FIRST
	// comment author is what decides.
	PRAuthor string
}

type gqlComment struct {
	Author string
	Body   string
}

// gqlMock serves POST /graphql for the review-thread code paths and records
// every mutation it receives. Queries are recognised by a substring of the
// operation text — good enough for a fixture, and it keeps the mock honest
// about which operation the code actually sent.
type gqlMock struct {
	mu       sync.Mutex
	threads  map[string]*gqlThread // by thread id
	prs      map[string][]string   // "owner/name#number" → thread ids in order
	queries  int
	resolved []string
	replies  map[string][]string // thread id → bodies
	// failWith, when non-empty, is returned as a GraphQL error for every call.
	failWith string
}

func newGQLMock() *gqlMock {
	return &gqlMock{threads: map[string]*gqlThread{}, prs: map[string][]string{}, replies: map[string][]string{}}
}

func (m *gqlMock) add(t *gqlThread) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.threads[t.ID] = t
	key := t.RepoOwner + "/" + t.RepoName + "#" + strconv.Itoa(t.PRNumber)
	m.prs[key] = append(m.prs[key], t.ID)
}

func (m *gqlMock) renderThread(t *gqlThread) map[string]any {
	comments := make([]map[string]any, 0, len(t.Comments))
	for i, c := range t.Comments {
		comments = append(comments, map[string]any{
			"author":     map[string]any{"login": c.Author},
			"body":       c.Body,
			"databaseId": t.DatabaseID + int64(i),
		})
	}
	var line any
	if t.Line > 0 {
		line = t.Line
	}
	return map[string]any{
		"id": t.ID, "isResolved": t.Resolved, "isOutdated": t.Outdated,
		"path": t.Path, "line": line,
		"comments": map[string]any{"nodes": comments},
		"pullRequest": map[string]any{
			"number":     t.PRNumber,
			"author":     map[string]any{"login": t.PRAuthor},
			"repository": map[string]any{"nameWithOwner": t.RepoOwner + "/" + t.RepoName},
		},
	}
}

func (m *gqlMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/graphql") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("mock: bad graphql body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.queries++
		w.Header().Set("Content-Type", "application/json")
		if m.failWith != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []map[string]any{{"message": m.failWith}}})
			return
		}
		var data any
		switch {
		case strings.Contains(req.Query, "resolveReviewThread("):
			id, _ := req.Variables["id"].(string)
			th := m.threads[id]
			if th == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to a node with the global id of '" + id + "'"}}})
				return
			}
			th.Resolved = true
			m.resolved = append(m.resolved, id)
			data = map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": id, "isResolved": true}}}
		case strings.Contains(req.Query, "addPullRequestReviewThreadReply("):
			id, _ := req.Variables["id"].(string)
			body, _ := req.Variables["body"].(string)
			th := m.threads[id]
			if th == nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": "NOT_FOUND", "message": "Could not resolve to a node"}}})
				return
			}
			th.Comments = append(th.Comments, gqlComment{Author: "hive[bot]", Body: body})
			m.replies[id] = append(m.replies[id], body)
			data = map[string]any{"addPullRequestReviewThreadReply": map[string]any{"comment": map[string]any{"id": "PRRC_new"}}}
		case strings.Contains(req.Query, "node(id:"):
			id, _ := req.Variables["id"].(string)
			th := m.threads[id]
			if th == nil {
				data = map[string]any{"node": nil}
				break
			}
			data = map[string]any{"node": m.renderThread(th)}
		case strings.Contains(req.Query, "reviewThreads("):
			owner, _ := req.Variables["owner"].(string)
			repo, _ := req.Variables["repo"].(string)
			num, _ := req.Variables["number"].(float64)
			key := owner + "/" + repo + "#" + strconv.Itoa(int(num))
			ids, ok := m.prs[key]
			if !ok {
				data = map[string]any{"repository": map[string]any{"pullRequest": nil}}
				break
			}
			nodes := make([]map[string]any, 0, len(ids))
			head := ""
			for _, id := range ids {
				th := m.threads[id]
				nodes = append(nodes, m.renderThread(th))
				head = th.HeadRef
			}
			data = map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"headRefName": head,
				"reviewThreads": map[string]any{
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					"nodes":    nodes,
				},
			}}}
		default:
			t.Errorf("mock: unrecognised graphql operation: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}
}

func (m *gqlMock) queryCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queries
}

var testBots = config.ReviewBotsConfig{Logins: []string{"chatgpt-codex-connector[bot]", "Copilot"}}

func isHiveTest(login string) bool { return strings.EqualFold(login, "hive[bot]") }

func rawThread(id string, resolved, outdated bool, comments ...gqlComment) rawReviewThread {
	t := rawReviewThread{ID: id, IsResolved: resolved, IsOutdated: outdated, Path: "src/x.go"}
	line := 42
	t.Line = &line
	for i, c := range comments {
		var rc rawReviewComment
		rc.Author.Login = c.Author
		rc.Body = c.Body
		rc.DatabaseID = int64(1000 + i)
		t.Comments.Nodes = append(t.Comments.Nodes, rc)
	}
	return t
}

// The #7360 thread filter: resolved, outdated, human-first-comment, and
// attempt-limit-reached threads are all excluded; an open bot thread with
// fewer hive replies than the cap passes with its location and body.
func TestFilterReviewThreads(t *testing.T) {
	bot := gqlComment{Author: "chatgpt-codex-connector[bot]", Body: "x may be nil"}
	hive := gqlComment{Author: "hive[bot]", Body: "fixed"}
	human := gqlComment{Author: "alice", Body: "please rename"}
	threads := []rawReviewThread{
		rawThread("PRRT_open", false, false, bot),
		rawThread("PRRT_resolved", true, false, bot),
		rawThread("PRRT_outdated", false, true, bot),
		rawThread("PRRT_human", false, false, human, bot),       // human opened it; a bot replying does not make it a bot thread
		rawThread("PRRT_attempted", false, false, bot, hive),    // 1 hive reply == default cap
		rawThread("PRRT_human_reply", false, false, bot, human), // a HUMAN reply is not an attempt
		rawThread("PRRT_copilot", false, false, gqlComment{Author: "COPILOT", Body: strings.Repeat("long ", 400)}),
		rawThread("PRRT_empty", false, false),
	}
	got := filterReviewThreads(threads, testBots, isHiveTest)
	ids := map[string]ReviewThread{}
	for _, th := range got {
		ids[th.ThreadID] = th
	}
	for _, want := range []string{"PRRT_open", "PRRT_human_reply", "PRRT_copilot"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("%s should pass the filter; got %v", want, threadIDs(ids))
		}
	}
	for _, reject := range []string{"PRRT_resolved", "PRRT_outdated", "PRRT_human", "PRRT_attempted", "PRRT_empty"} {
		if _, ok := ids[reject]; ok {
			t.Errorf("%s must be excluded", reject)
		}
	}
	if open := ids["PRRT_open"]; open.Path != "src/x.go" || open.Line != 42 || open.Author != "chatgpt-codex-connector[bot]" || open.Body != "x may be nil" || open.CommentID != 1000 || open.HiveReplies != 0 {
		t.Errorf("unexpected projection: %+v", open)
	}
	if long := ids["PRRT_copilot"]; len([]rune(long.Body)) > reviewThreadBodyRunes+1 || !strings.HasSuffix(long.Body, "…") {
		t.Errorf("body must be truncated to %d runes, got %d", reviewThreadBodyRunes, len([]rune(long.Body)))
	}

	// A higher cap admits an already-attempted thread once more, and a nil
	// isHive counts nothing as an attempt.
	two := testBots
	two.MaxAttemptsPerThread = 2
	if got := filterReviewThreads(threads[4:5], two, isHiveTest); len(got) != 1 || got[0].HiveReplies != 1 {
		t.Errorf("cap=2 should admit a once-attempted thread: %+v", got)
	}
	if got := filterReviewThreads(threads[4:5], testBots, nil); len(got) != 1 {
		t.Errorf("nil isHive must not count attempts: %+v", got)
	}

	// Feature off: nothing passes, whatever the threads look like.
	if got := filterReviewThreads(threads, config.ReviewBotsConfig{}, isHiveTest); got != nil {
		t.Errorf("disabled config must filter everything, got %v", got)
	}
}

func threadIDs(m map[string]ReviewThread) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestGraphQLEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com/":         "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:1234/":          "http://127.0.0.1:1234/graphql",
	}
	for in, want := range cases {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := graphQLEndpoint(u); got != want {
			t.Errorf("graphQLEndpoint(%s) = %s, want %s", in, got, want)
		}
	}
	if got := graphQLEndpoint(nil); got != "https://api.github.com/graphql" {
		t.Errorf("nil base = %s", got)
	}
}

func reviewThreadTestClient(t *testing.T, srvURL string, bots config.ReviewBotsConfig) *Client {
	t.Helper()
	c := testClient(t, srvURL)
	c.SetAppBotLogin("hive[bot]")
	c.SetReviewBots(bots)
	return c
}

// CollectReviewThreads lists only hive-authored, non-held PRs; a PR with no
// bot thread at all is not listed; a PR whose bot threads are all resolved is
// listed with zero threads; a human's thread never appears anywhere.
func TestCollectReviewThreads(t *testing.T) {
	mock := newGQLMock()
	bot := gqlComment{Author: "chatgpt-codex-connector[bot]", Body: "nil deref"}
	mock.add(&gqlThread{ID: "PRRT_1", Path: "a.go", Line: 3, Comments: []gqlComment{bot}, PRNumber: 1, RepoOwner: "o", RepoName: "r", HeadRef: "hive/fix-1", DatabaseID: 10})
	mock.add(&gqlThread{ID: "PRRT_1h", Path: "a.go", Line: 9, Comments: []gqlComment{{Author: "alice", Body: "rename"}}, PRNumber: 1, RepoOwner: "o", RepoName: "r", HeadRef: "hive/fix-1", DatabaseID: 20})
	mock.add(&gqlThread{ID: "PRRT_2", Resolved: true, Comments: []gqlComment{bot, {Author: "hive[bot]", Body: "done"}}, PRNumber: 2, RepoOwner: "o", RepoName: "r", HeadRef: "hive/fix-2", DatabaseID: 30})
	mock.add(&gqlThread{ID: "PRRT_3", Comments: []gqlComment{{Author: "bob", Body: "human only"}}, PRNumber: 3, RepoOwner: "o", RepoName: "r", HeadRef: "hive/fix-3", DatabaseID: 40})
	mock.add(&gqlThread{ID: "PRRT_4", Comments: []gqlComment{bot}, PRNumber: 4, RepoOwner: "o", RepoName: "r", HeadRef: "human/branch", DatabaseID: 50})
	mock.add(&gqlThread{ID: "PRRT_5", Comments: []gqlComment{bot}, PRNumber: 5, RepoOwner: "o", RepoName: "r", HeadRef: "hive/held", DatabaseID: 60})
	mock.add(&gqlThread{ID: "PRRT_7", Comments: []gqlComment{bot}, PRNumber: 7, RepoOwner: "o", RepoName: "r", HeadRef: "hive/blocked", DatabaseID: 70})
	// PR 8 is the hivecommons/hive#7638 case: a hive agent opened it on the
	// operator's own credentials, so GitHub shows a human author, but the body
	// carries the `— hive:` trailer. Its bot thread must be listed; the human
	// thread beside it must not. PR 4 (same author, no trailer) stays dropped.
	mock.add(&gqlThread{ID: "PRRT_8", Path: "docs/perm.md", Line: 12, Comments: []gqlComment{{Author: "chatgpt-codex-connector[bot]", Body: "Update the permission note"}}, PRNumber: 8, RepoOwner: "o", RepoName: "r", HeadRef: "relay/fix-8", DatabaseID: 80})
	mock.add(&gqlThread{ID: "PRRT_8h", Path: "docs/perm.md", Line: 30, Comments: []gqlComment{{Author: "dave", Body: "typo"}}, PRNumber: 8, RepoOwner: "o", RepoName: "r", HeadRef: "relay/fix-8", DatabaseID: 90})
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	c := reviewThreadTestClient(t, srv.URL, testBots)

	prs := []PullRequest{
		{Repo: "r", Number: 1, Title: "fix one", Author: "hive[bot]"},
		{Repo: "r", Number: 2, Title: "fix two", Author: "hive[bot]"},
		{Repo: "r", Number: 3, Title: "fix three", Author: "hive[bot]"},
		{Repo: "r", Number: 4, Title: "human PR", Author: "carol"},
		{Repo: "r", Number: 5, Title: "held", Author: "hive[bot]", Labels: []string{"hold"}},
		{Repo: "r", Number: 6, Title: "draft", Author: "hive[bot]", Draft: true},
		{Repo: "r", Number: 7, Title: "blocked", Author: "hive[bot]", Labels: []string{"Blocked"}},
		{Repo: "r", Number: 8, Title: "relay PR", Author: "carol", HiveAttributed: true},
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	report := c.CollectReviewThreads(context.Background(), prs, now)
	if !report.Enabled || report.GeneratedAt != "2026-09-17T12:00:00Z" {
		t.Errorf("header wrong: %+v", report)
	}
	if report.TotalThreads != 2 || len(report.PRs) != 3 {
		t.Fatalf("expected 3 listed PRs / 2 threads, got %d PRs / %d threads: %+v", len(report.PRs), report.TotalThreads, report.PRs)
	}
	one, two, eight := report.PRs[0], report.PRs[1], report.PRs[2]
	if one.Repo != "o/r" || one.Number != 1 || one.HeadRef != "hive/fix-1" || len(one.Threads) != 1 || one.Threads[0].ThreadID != "PRRT_1" || one.Title != "fix one" {
		t.Errorf("PR 1 wrong: %+v", one)
	}
	if two.Number != 2 || len(two.Threads) != 0 || two.HeadRef != "hive/fix-2" {
		t.Errorf("PR 2 (all resolved) must be listed with zero threads: %+v", two)
	}
	if eight.Number != 8 || eight.HeadRef != "relay/fix-8" || len(eight.Threads) != 1 || eight.Threads[0].ThreadID != "PRRT_8" || eight.Agent != "" {
		t.Errorf("PR 8 (human author, hive trailer) must be listed with its bot thread and no agent: %+v", eight)
	}
	raw, _ := json.Marshal(report)
	for _, human := range []string{"PRRT_1h", "PRRT_3", "alice", "bob", "PRRT_4", "PRRT_5", "PRRT_7", "PRRT_8h", "dave"} {
		if strings.Contains(string(raw), human) {
			t.Errorf("report leaks %q (human thread / non-hive PR / held or blocked PR):\n%s", human, raw)
		}
	}
	// Only the hive-mediated, non-held, non-draft PRs (1, 2, 3, 8) cost a
	// query; the trailer-less human PR (4) never reaches GraphQL.
	if got := mock.queryCount(); got != 4 {
		t.Errorf("expected 4 GraphQL queries, got %d", got)
	}
}

// isHiveMediatedPR: a hive login as author OR the attribution trailer in the
// body qualifies; a human author with no trailer does not, and neither does an
// empty author (hivecommons/hive#7638).
func TestIsHiveMediatedPR(t *testing.T) {
	c := reviewThreadTestClient(t, "http://127.0.0.1:0/", testBots)
	cases := []struct {
		name string
		pr   PullRequest
		want bool
	}{
		{"App bot author", PullRequest{Author: "hive[bot]"}, true},
		{"App bot author, different case", PullRequest{Author: "HIVE[BOT]"}, true},
		{"human author, trailer in body", PullRequest{Author: "carol", HiveAttributed: true}, true},
		{"human author, no trailer", PullRequest{Author: "carol"}, false},
		{"no author, no trailer", PullRequest{}, false},
		{"no author, trailer", PullRequest{HiveAttributed: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.isHiveMediatedPR(tc.pr); got != tc.want {
				t.Errorf("isHiveMediatedPR(%+v) = %v, want %v", tc.pr, got, tc.want)
			}
		})
	}
}

// Feature off: the report is empty and marked disabled, with no API traffic.
func TestCollectReviewThreads_Disabled(t *testing.T) {
	mock := newGQLMock()
	mock.add(&gqlThread{ID: "PRRT_1", Comments: []gqlComment{{Author: "Copilot", Body: "x"}}, PRNumber: 1, RepoOwner: "o", RepoName: "r"})
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	c := reviewThreadTestClient(t, srv.URL, config.ReviewBotsConfig{})
	report := c.CollectReviewThreads(context.Background(), []PullRequest{{Repo: "r", Number: 1, Author: "hive[bot]"}}, time.Now())
	if report.Enabled || len(report.PRs) != 0 || report.TotalThreads != 0 || report.PRs == nil {
		t.Errorf("disabled report wrong: %+v", report)
	}
	if mock.queryCount() != 0 {
		t.Errorf("disabled feature must make no GraphQL calls, got %d", mock.queryCount())
	}
}

// A failed thread fetch skips that PR (no attempt this pass) without
// poisoning the rest of the report.
func TestCollectReviewThreads_FetchFailureSkipsPR(t *testing.T) {
	mock := newGQLMock()
	mock.failWith = "Something went wrong"
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	c := reviewThreadTestClient(t, srv.URL, testBots)
	report := c.CollectReviewThreads(context.Background(), []PullRequest{{Repo: "r", Number: 1, Author: "hive[bot]"}}, time.Now())
	if !report.Enabled || len(report.PRs) != 0 {
		t.Errorf("failed PR must be omitted, report still enabled: %+v", report)
	}
}

// The report round-trips through the artifact path, and an empty report is
// still written as a well-formed file.
func TestWriteReadReviewThreadsReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics", "review-threads.json")
	in := ReviewThreadsReport{GeneratedAt: "2026-09-17T00:00:00Z", Enabled: true, TotalThreads: 1, PRs: []ReviewThreadPR{{
		Repo: "o/r", Number: 7, HeadRef: "hive/x", Agent: "quality",
		Threads: []ReviewThread{{ThreadID: "PRRT_a", Path: "p.go", Line: 1, Author: "Copilot", Body: "b"}},
	}}}
	if err := WriteReviewThreadsReport(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadReviewThreadsReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.TotalThreads != 1 || len(out.PRs) != 1 || out.PRs[0].Agent != "quality" || out.PRs[0].Threads[0].ThreadID != "PRRT_a" {
		t.Errorf("round-trip mismatch: %+v", out)
	}
	if err := WriteReviewThreadsReport(path, ReviewThreadsReport{}); err != nil {
		t.Fatal(err)
	}
	out, err = ReadReviewThreadsReport(path)
	if err != nil || out.PRs == nil || len(out.PRs) != 0 {
		t.Errorf("empty report must read back as an empty list: %+v, %v", out, err)
	}
}

func TestSameRepoRef(t *testing.T) {
	cases := []struct {
		nameWithOwner, requested, org string
		want                          bool
	}{
		{"o/r", "o/r", "o", true},
		{"o/r", "r", "o", true},
		{"O/R", "o/r", "", true},
		{"o/r", "r", "", true},
		{"o/r", "x/r", "o", false},
		{"o/r", "r2", "o", false},
		{"", "r", "o", false},
		{"o/r", "", "o", false},
	}
	for _, tc := range cases {
		if got := sameRepoRef(tc.nameWithOwner, tc.requested, tc.org); got != tc.want {
			t.Errorf("sameRepoRef(%q,%q,%q) = %v, want %v", tc.nameWithOwner, tc.requested, tc.org, got, tc.want)
		}
	}
}
