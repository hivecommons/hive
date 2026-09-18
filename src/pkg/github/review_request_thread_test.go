package github

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// threadWatcherFixture builds a mock forge with one bot thread (PRRT_bot) and
// one human thread (PRRT_human) on o/r#5, an already-resolved bot thread
// (PRRT_done), and a bot thread the hive already replied in (PRRT_tried).
func threadWatcherFixture(t *testing.T, bots config.ReviewBotsConfig) (*gqlMock, *Client, string) {
	t.Helper()
	mock := newGQLMock()
	bot := gqlComment{Author: "chatgpt-codex-connector[bot]", Body: "nil deref"}
	mock.add(&gqlThread{ID: "PRRT_bot", Path: "a.go", Line: 1, Comments: []gqlComment{bot}, PRNumber: 5, RepoOwner: "o", RepoName: "r"})
	mock.add(&gqlThread{ID: "PRRT_human", Path: "a.go", Line: 2, Comments: []gqlComment{{Author: "alice", Body: "rename this"}}, PRNumber: 5, RepoOwner: "o", RepoName: "r"})
	mock.add(&gqlThread{ID: "PRRT_done", Resolved: true, Comments: []gqlComment{bot}, PRNumber: 5, RepoOwner: "o", RepoName: "r"})
	mock.add(&gqlThread{ID: "PRRT_tried", Comments: []gqlComment{bot, {Author: "hive[bot]", Body: "tried once"}}, PRNumber: 5, RepoOwner: "o", RepoName: "r"})
	mock.add(&gqlThread{ID: "PRRT_other", Comments: []gqlComment{bot}, PRNumber: 6, RepoOwner: "o", RepoName: "r"})
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	c.SetAppBotLogin("hive[bot]")
	c.SetReviewBots(bots)
	dir := withReviewDir(t)
	return mock, c, dir
}

func readReviewResult(t *testing.T, reqPath string) ReviewResponse {
	t.Helper()
	data, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file: %v", err)
	}
	var resp ReviewResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("result json: %v", err)
	}
	return resp
}

// Happy path: a resolve_thread request on an open bot thread resolves it,
// audits as agent_pr_reviewed state=thread_resolved, and is consumed.
func TestReviewRequestWatcher_ResolveThread(t *testing.T) {
	mock, c, dir := threadWatcherFixture(t, testBots)
	var gotAction, gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) { gotAction, gotDetail = action, detail })

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if len(mock.resolved) != 1 || mock.resolved[0] != "PRRT_bot" {
		t.Fatalf("expected PRRT_bot resolved, got %v", mock.resolved)
	}
	if gotAction != AuditActionPRReviewed || !strings.Contains(gotDetail, "state=thread_resolved") || !strings.Contains(gotDetail, "thread=PRRT_bot") {
		t.Errorf("audit wrong: %s %q", gotAction, gotDetail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("request must be consumed on success")
	}
	resp := readReviewResult(t, reqPath)
	if !resp.OK || resp.State != "thread_resolved" || resp.ThreadID != "PRRT_bot" || resp.Number != 5 {
		t.Errorf("result wrong: %+v", resp)
	}
}

// Happy path: a comment request carrying thread_id becomes an in-thread
// reply (GraphQL addPullRequestReviewThreadReply), NOT a PR-level review.
func TestReviewRequestWatcher_ThreadReply(t *testing.T) {
	mock, c, dir := threadWatcherFixture(t, testBots)
	var gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) { gotDetail = detail })

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "r", Number: 5, Event: "comment", ThreadID: "PRRT_bot", Body: "Added a nil guard in a.go.", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if got := mock.replies["PRRT_bot"]; len(got) != 1 || !strings.HasPrefix(got[0], "Added a nil guard in a.go.") {
		t.Fatalf("expected one in-thread reply, got %v", got)
	}
	if len(mock.resolved) != 0 {
		t.Error("a reply must not resolve the thread")
	}
	if !strings.Contains(gotDetail, "state=thread_replied") {
		t.Errorf("audit detail wrong: %q", gotDetail)
	}
	if resp := readReviewResult(t, reqPath); !resp.OK || resp.State != "thread_replied" {
		t.Errorf("result wrong: %+v", resp)
	}
	// The mock appended the hive's reply, so the thread is now at the
	// attempt cap: a second reply is denied, while resolving still works.
	reqPath2, _ := WriteReviewRequest(dir, ReviewRequest{Repo: "r", Number: 5, Event: "comment", ThreadID: "PRRT_bot", Body: "again", Agent: "scanner"})
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath2 + ".denied"); err != nil {
		t.Error("second reply past max_attempts_per_thread must be denied")
	}
	if len(mock.replies["PRRT_bot"]) != 1 {
		t.Errorf("no second reply may land, got %v", mock.replies["PRRT_bot"])
	}
	reqPath3, _ := WriteReviewRequest(dir, ReviewRequest{Repo: "r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"})
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath3); !os.IsNotExist(err) {
		t.Error("resolve after the reply must still succeed (attempt cap applies to replies only)")
	}
}

// The guard denials (hivecommons/hive#7360 acceptance): a human's thread, an
// already-resolved thread, a thread on another PR, an unknown id, a thread
// already at the attempt cap (reply), a missing thread_id, and a hive with
// review_bots off. Every one quarantines as .denied, touches nothing on the
// forge, and records the reason in the result file.
func TestReviewRequestWatcher_ThreadGuardDenials(t *testing.T) {
	cases := []struct {
		name   string
		bots   config.ReviewBotsConfig
		req    ReviewRequest
		reason string
		bad    bool // shape error → .bad instead of .denied
	}{
		{name: "human thread resolve", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_human", Agent: "scanner"},
			reason: "opened by alice"},
		{name: "human thread reply", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", ThreadID: "PRRT_human", Body: "done", Agent: "scanner"},
			reason: "not a configured review bot"},
		{name: "already resolved", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_done", Agent: "scanner"},
			reason: "already resolved"},
		{name: "wrong PR", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_other", Agent: "scanner"},
			reason: "belongs to o/r#6"},
		{name: "unknown id", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_nope", Agent: "scanner"},
			reason: "not found"},
		{name: "attempts exhausted (reply)", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", ThreadID: "PRRT_tried", Body: "again", Agent: "scanner"},
			reason: "max_attempts_per_thread=1"},
		{name: "feature off", bots: config.ReviewBotsConfig{},
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"},
			reason: "review_bots is not configured"},
		{name: "missing thread_id", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", Agent: "scanner"},
			reason: "resolve_thread requires thread_id", bad: true},
		{name: "thread_id on approve", bots: testBots,
			req:    ReviewRequest{Repo: "o/r", Number: 5, Event: "approve", ThreadID: "PRRT_bot", Agent: "scanner"},
			reason: "only valid with event comment", bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, c, dir := threadWatcherFixture(t, tc.bots)
			audited := false
			c.SetAttributionAudit(func(action, detail, agent string) { audited = true })
			reqPath, err := WriteReviewRequest(dir, tc.req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessReviewRequestsOnce(context.Background())

			suffix := ".denied"
			if tc.bad {
				suffix = ".bad"
			}
			if _, err := os.Stat(reqPath + suffix); err != nil {
				t.Fatalf("expected %s quarantine: %v", suffix, err)
			}
			if len(mock.resolved) != 0 || len(mock.replies) != 0 {
				t.Errorf("forge must be untouched: resolved=%v replies=%v", mock.resolved, mock.replies)
			}
			if audited {
				t.Error("a denial must not produce an agent_pr_reviewed audit entry")
			}
			resp := readReviewResult(t, reqPath)
			if resp.OK || !strings.Contains(resp.Error, tc.reason) {
				t.Errorf("result should carry the reason %q, got %+v", tc.reason, resp)
			}
		})
	}
}

// hivecommons/hive#7638: the guard keys on the THREAD's first author, never
// the PR's. On a PR a hive agent opened on the operator's own credentials —
// GitHub shows a human as author — a resolve on the Codex thread goes through,
// while a resolve on the human's thread beside it is still refused. If the
// guard ever grew a PR-author check, every relay PR the monitor now lists
// would be routed to an agent whose requests are all denied.
func TestReviewRequestWatcher_ThreadGuardIgnoresPRAuthor(t *testing.T) {
	mock := newGQLMock()
	codex := gqlComment{Author: "chatgpt-codex-connector[bot]", Body: "Update the permission note for the newly refused options"}
	mock.add(&gqlThread{ID: "PRRT_relay_bot", Path: "docs/perm.md", Line: 12, Comments: []gqlComment{codex}, PRNumber: 195, RepoOwner: "o", RepoName: "r", PRAuthor: "danathar"})
	mock.add(&gqlThread{ID: "PRRT_relay_human", Path: "docs/perm.md", Line: 30, Comments: []gqlComment{{Author: "danathar", Body: "leave this"}}, PRNumber: 195, RepoOwner: "o", RepoName: "r", PRAuthor: "danathar"})
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	c.SetAppBotLogin("hive[bot]")
	c.SetReviewBots(testBots)
	dir := withReviewDir(t)

	botReq, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 195, Event: "resolve_thread", ThreadID: "PRRT_relay_bot", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if len(mock.resolved) != 1 || mock.resolved[0] != "PRRT_relay_bot" {
		t.Fatalf("bot thread on a human-authored relay PR must resolve, got %v", mock.resolved)
	}
	if resp := readReviewResult(t, botReq); !resp.OK || resp.State != "thread_resolved" {
		t.Errorf("result wrong: %+v", resp)
	}

	humanReq, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 195, Event: "resolve_thread", ThreadID: "PRRT_relay_human", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(humanReq + ".denied"); err != nil {
		t.Fatalf("the PR author's own thread must still be denied: %v", err)
	}
	if len(mock.resolved) != 1 {
		t.Errorf("human thread must stay open, resolved=%v", mock.resolved)
	}
	if resp := readReviewResult(t, humanReq); resp.OK || !strings.Contains(resp.Error, "opened by danathar") {
		t.Errorf("denial should name the human author, got %+v", resp)
	}
}

// A thread request still goes through the per-agent authorizer: a nil
// authorizer (fail closed) denies before the guard is even consulted.
func TestReviewRequestWatcher_ThreadRequestFailsClosedWithoutAuthz(t *testing.T) {
	mock, c, dir := threadWatcherFixture(t, testBots)
	c.reviewAuthz = nil
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("nil authorizer must deny: %v", err)
	}
	if mock.queryCount() != 0 {
		t.Errorf("no forge call may happen before authorization, got %d", mock.queryCount())
	}
}

// A forge failure on the guard's re-fetch is transient: the request survives
// for a backoff retry instead of being denied or destroyed.
func TestReviewRequestWatcher_ThreadFetchFailureRetries(t *testing.T) {
	mock, c, dir := threadWatcherFixture(t, testBots)
	mock.failWith = "Something went wrong while executing your query"
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request must survive a transient forge failure: %v", err)
	}
	if resp := readReviewResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "Something went wrong") {
		t.Errorf("result should record the transient error: %+v", resp)
	}
}
