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
	"sync"
	"testing"
)

// testBotPRAuthor is a bot login: a PR it opened is in the set the swarm
// reviews without review.all_authors, so formal verdicts pass through.
const testBotPRAuthor = "some-app[bot]"

// testContributorLogin is a person who is not this hive.
const testContributorLogin = "outside-dev"

// servePRAuthor answers GET /repos/{o}/{r}/pulls/{n} with a PR opened by
// login and reports whether it handled the request. An empty login serves a
// PR with no author, the "unknown" case.
func servePRAuthor(w http.ResponseWriter, r *http.Request, login string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/repos/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	const ownerRepoPullsNumber = 4
	if len(parts) != ownerRepoPullsNumber || parts[2] != "pulls" {
		return false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return false
	}
	userType := "User"
	if strings.HasSuffix(login, "[bot]") {
		userType = "Bot"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"number": n,
		"user":   map[string]string{"login": login, "type": userType},
	})
	return true
}

// Every spelling the relay accepts for a formal verdict must come out as
// COMMENT on a contributor PR, and exactly as reviewEventToAPI maps it on a
// hive-authored one.
func TestContributorSafeReviewEvent(t *testing.T) {
	cases := []struct {
		event    string
		intended string // "" = not a formal verdict
	}{
		{"approve", "approved"},
		{"APPROVE", "approved"},
		{" Approve ", "approved"},
		{"approved", "approved"},
		{"Approved", "approved"},
		{"request_changes", "changes_requested"},
		{"REQUEST_CHANGES", "changes_requested"},
		{"request-changes", "changes_requested"},
		{"Request-Changes", "changes_requested"},
		{"changes_requested", "changes_requested"},
		{" CHANGES_REQUESTED\n", "changes_requested"},
		{"comment", ""},
		{"Commented", ""},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.event), func(t *testing.T) {
			apiEvent, state, intended, ok := contributorSafeReviewEvent(tc.event, false)
			if !ok {
				t.Fatalf("contributor: %q not accepted", tc.event)
			}
			if apiEvent != reviewAPIEventComment || state != reviewStateCommented {
				t.Errorf("contributor: %q -> %s/%s, want COMMENT/commented", tc.event, apiEvent, state)
			}
			if isFormalReviewVerdict(apiEvent) {
				t.Errorf("contributor: %q produced a formal verdict %s", tc.event, apiEvent)
			}
			if intended != tc.intended {
				t.Errorf("contributor: %q intended = %q, want %q", tc.event, intended, tc.intended)
			}

			wantAPI, wantState, _ := reviewEventToAPI(tc.event)
			apiEvent, state, intended, ok = contributorSafeReviewEvent(tc.event, true)
			if !ok || apiEvent != wantAPI || state != wantState || intended != "" {
				t.Errorf("hive-authored: %q -> %s/%s intended=%q ok=%v, want %s/%s unchanged",
					tc.event, apiEvent, state, intended, ok, wantAPI, wantState)
			}
		})
	}
	for _, hive := range []bool{false, true} {
		if _, _, _, ok := contributorSafeReviewEvent("merge", hive); ok {
			t.Errorf("unknown event accepted (hiveAuthored=%v)", hive)
		}
	}
}

func TestContributorVerdictNote(t *testing.T) {
	approve := contributorVerdictNote("approved", "  looks fine  ")
	if !strings.Contains(approve, "Reviewer verdict: approve.") || !strings.HasSuffix(approve, "\n\nlooks fine") {
		t.Errorf("approve note wrong: %q", approve)
	}
	changes := contributorVerdictNote("changes_requested", "")
	if !strings.Contains(changes, "Reviewer verdict: request changes.") || strings.Contains(changes, "\n\n") {
		t.Errorf("request-changes note on an empty body wrong: %q", changes)
	}
	for _, note := range []string{approve, changes} {
		if strings.Contains(note, "@") {
			t.Errorf("note must carry no mention: %q", note)
		}
	}
}

// guardServer records every GitHub call the relay makes, answers the PR GET
// with author (or 404 when failLook), and accepts review POSTs.
type guardServer struct {
	mu       sync.Mutex
	calls    []string // "METHOD path"
	events   []string
	bodies   []string
	lookups  int
	author   string
	failLook bool
}

func (g *guardServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		if g.failLook && r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/reviews") {
			g.lookups++
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if servePRAuthor(w, r, g.author) {
			g.lookups++
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") {
			var body struct {
				Event string `json:"event"`
				Body  string `json:"body"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			g.events = append(g.events, body.Event)
			g.bodies = append(g.bodies, body.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

type guardRun struct {
	result ReviewResponse
	detail string
}

func runGuardedReview(t *testing.T, srv *guardServer, req ReviewRequest, setup func(*Client)) guardRun {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := reviewTestClient(t, ts.URL)
	if setup != nil {
		setup(c)
	}
	dir := withReviewDir(t)
	var detail string
	c.SetAttributionAudit(func(action, d, agent string) {
		if action == AuditActionPRReviewed {
			detail = d
		}
	})
	reqPath, err := WriteReviewRequest(dir, req)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("request not consumed: %v", err)
	}
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var res ReviewResponse
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return guardRun{result: res, detail: detail}
}

// assertOnlyCommentReview is the #9590 acceptance invariant: on a contributor
// PR the relay's only write is one COMMENT review. No approve, no request
// changes, and no merge, close, edit, label or push call of any kind.
func assertOnlyCommentReview(t *testing.T, g *guardServer) {
	t.Helper()
	if len(g.events) != 1 || g.events[0] != reviewAPIEventComment {
		t.Fatalf("review events = %v, want exactly one COMMENT", g.events)
	}
	for _, call := range g.calls {
		method, path, _ := strings.Cut(call, " ")
		if method == http.MethodGet {
			continue
		}
		if method != http.MethodPost || !strings.HasSuffix(path, "/reviews") {
			t.Errorf("contributor PR received a non-review write: %s", call)
		}
	}
}

// End to end: approve and request_changes, in every accepted spelling, on a
// person's PR are submitted as COMMENT, the body says what was intended, and
// the result file and audit trail both record the downgrade.
func TestReviewRelay_ContributorPRIsCommentOnly(t *testing.T) {
	cases := []struct {
		event    string
		body     string
		intended string
	}{
		{"approve", "", "approved"},
		{"APPROVED", "ship it", "approved"},
		{"request_changes", "fix the nil deref", "changes_requested"},
		{"Request-Changes", "fix the nil deref", "changes_requested"},
		{"CHANGES_REQUESTED", "fix the nil deref", "changes_requested"},
	}
	for i, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			g := &guardServer{author: testContributorLogin}
			run := runGuardedReview(t, g, ReviewRequest{
				Repo: "o/r", Number: 100 + i, Event: tc.event, Body: tc.body, Agent: "reviewer",
			}, func(c *Client) {
				c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-worker", AppLogin: "hive[bot]"})
			})
			assertOnlyCommentReview(t, g)
			if g.lookups != 1 {
				t.Errorf("author lookups = %d, want 1", g.lookups)
			}
			if !strings.HasPrefix(g.bodies[0], "_Reviewer verdict: ") {
				t.Errorf("posted body does not lead with the intended verdict: %q", g.bodies[0])
			}
			if tc.body != "" && !strings.Contains(g.bodies[0], tc.body) {
				t.Errorf("reviewer's body lost: %q", g.bodies[0])
			}
			if !run.result.OK || run.result.State != reviewStateCommented {
				t.Errorf("result = %+v, want ok state=commented", run.result)
			}
			if !strings.Contains(run.result.Note, "contributor PR: "+tc.intended) || !strings.Contains(run.result.Note, testContributorLogin) {
				t.Errorf("result note does not explain the downgrade: %q", run.result.Note)
			}
			for _, want := range []string{"state=commented", "requested_state=" + tc.intended, "downgraded=contributor_pr"} {
				if !strings.Contains(run.detail, want) {
					t.Errorf("audit detail missing %q: %q", want, run.detail)
				}
			}
		})
	}
}

// Fail closed: a PR whose author cannot be read, or has none, is treated as a
// contributor PR. The review still lands, as a comment.
func TestReviewRelay_UnknownAuthorIsCommentOnly(t *testing.T) {
	for name, g := range map[string]*guardServer{
		"lookup fails":   {failLook: true},
		"author missing": {author: ""},
	} {
		t.Run(name, func(t *testing.T) {
			run := runGuardedReview(t, g, ReviewRequest{
				Repo: "o/r", Number: 200, Event: "approve", Agent: "reviewer",
			}, nil)
			assertOnlyCommentReview(t, g)
			if !run.result.OK || !strings.Contains(run.result.Note, "treated as a contributor PR") {
				t.Errorf("result = %+v, want ok with a fail-closed note", run.result)
			}
		})
	}
}

// The hive's own PRs (App bot, project.ai_author in any case, another bot)
// keep their formal verdict and read exactly as before: no note, no extra
// audit keys, body untouched.
func TestReviewRelay_HiveAuthoredPRUnchanged(t *testing.T) {
	cases := []struct {
		author, event, wantAPI, wantState string
	}{
		{"hive[bot]", "approve", reviewAPIEventApprove, "approved"},
		{"Hive-Worker", "request_changes", reviewAPIEventRequestChanges, "changes_requested"},
		{testBotPRAuthor, "approve", reviewAPIEventApprove, "approved"},
	}
	for i, tc := range cases {
		t.Run(tc.author, func(t *testing.T) {
			g := &guardServer{author: tc.author}
			run := runGuardedReview(t, g, ReviewRequest{
				Repo: "o/r", Number: 300 + i, Event: tc.event, Body: "the review", Agent: "reviewer",
			}, func(c *Client) {
				c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-worker"})
				c.SetAppBotLogin("hive[bot]")
			})
			if len(g.events) != 1 || g.events[0] != tc.wantAPI {
				t.Fatalf("events = %v, want [%s]", g.events, tc.wantAPI)
			}
			if strings.Contains(g.bodies[0], "Reviewer verdict:") || !strings.HasPrefix(g.bodies[0], "the review") {
				t.Errorf("hive-authored body altered: %q", g.bodies[0])
			}
			if run.result.State != tc.wantState || run.result.Note != "" {
				t.Errorf("result = %+v, want state=%s and no note", run.result, tc.wantState)
			}
			if strings.Contains(run.detail, "requested_state") || !strings.Contains(run.detail, "state="+tc.wantState) {
				t.Errorf("audit detail = %q", run.detail)
			}
		})
	}
}

// A plain comment is already safe: no author lookup, nothing rewritten.
func TestReviewRelay_CommentSkipsAuthorLookup(t *testing.T) {
	g := &guardServer{author: testContributorLogin}
	run := runGuardedReview(t, g, ReviewRequest{
		Repo: "o/r", Number: 400, Event: "comment", Body: "a note", Agent: "reviewer",
	}, nil)
	assertOnlyCommentReview(t, g)
	if g.lookups != 0 {
		t.Errorf("comment review made %d author lookups, want 0", g.lookups)
	}
	if strings.Contains(g.bodies[0], "Reviewer verdict:") || run.result.Note != "" {
		t.Errorf("comment review was rewritten: body=%q note=%q", g.bodies[0], run.result.Note)
	}
}
