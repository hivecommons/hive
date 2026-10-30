package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// repoItemDecisionMock is the slice of GitHub the decision endpoint touches:
// permission lookups, one issue (#7, parked) and one PR (#8), label writes,
// comments and the close edit.
type repoItemDecisionMock struct {
	mu          sync.Mutex
	issueLabels []string
	added       []string
	removed     []string
	comments    []string
	edits       []map[string]any
	// prForeign makes PR #8 look like a human opened it; prLabels are the
	// labels pulls/8 reports.
	prForeign bool
	prLabels  []string
	// prMergeState is PR #8's mergeable_state ("" = clean); prRedCI and
	// prNoCI shape the head's combined status when the state alone does not
	// settle CI (e.g. "blocked").
	prFork bool
	// prNoMaintainerEdits clears maintainer_can_modify on PR #8.
	prNoMaintainerEdits bool
	workflowPath        string
	prMergeState        string
	updates             int
	prRedCI             bool
	prNoCI              bool
	// prTidePending is hivecommons/hive#15722's head: every check green,
	// only the tide merge-metadata context pending (GitHub: "unstable").
	prTidePending bool
	// priorComments are the comment bodies GET /comments lists (a prior
	// delegated decision makes a repeat click a no-op).
	priorComments []string
	// merges records PUT /pulls/8/merge bodies; reviews the POST /reviews
	// bodies (merge-now / queue-merge, hivecommons/hive#15078).
	merges  []map[string]any
	reviews []map[string]any
	// supersederState / supersederMerged shape GET pulls/90, the hive copy
	// an adoption marker on #8 may name (hivecommons/hive#15052); "" is 404.
	supersederState  string
	supersederMerged bool
	// timeline is what GET /issues/N/timeline answers (nil = empty): the
	// re-park read of hivecommons/hive#15021.
	timeline []map[string]any
	// commitBy maps a head SHA to the login GET /commits/{sha} reports as
	// its committer; an unlisted SHA reads as a person's push.
	commitBy map[string]string
	// prBody is PR #8's body; fixedIssueClosedBy > 0 makes GET /issues/42
	// a closed issue whose closedByPullRequestsReferences names that merged
	// PR (GET /pulls/<n> answers merged with a merge_commit_sha): the
	// already-fixed rung of hivecommons/hive#15289.
	prBody             string
	fixedIssueClosedBy int
	// issueTitle / issueBody shape issue #7 (empty = untitled, no body).
	issueTitle, issueBody string
}

func newRepoItemDecisionServer(t *testing.T, m *repoItemDecisionMock) *Server {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && (strings.HasSuffix(path, "/collaborators/writer/permission") || strings.HasSuffix(path, "/collaborators/someone/permission")):
			_, _ = io.WriteString(w, `{"permission":"write"}`)
		case r.Method == http.MethodPost && strings.Contains(path, "/statuses/"):
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/collaborators/reader/permission"):
			_, _ = io.WriteString(w, `{"permission":"read"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues/7"):
			labels := make([]map[string]string, 0, len(m.issueLabels))
			for _, label := range m.issueLabels {
				labels = append(labels, map[string]string{"name": label})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "title": m.issueTitle, "body": m.issueBody, "labels": labels, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && (strings.HasSuffix(path, "/issues/8") || strings.HasSuffix(path, "/issues/9")):
			n := 8
			if strings.HasSuffix(path, "/issues/9") {
				n = 9
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "labels": []any{}, "pull_request": map[string]any{"url": "x"}, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues/42"):
			state := "open"
			if m.fixedIssueClosedBy > 0 {
				state = "closed"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": state, "labels": []any{}, "user": map[string]any{"login": "someone", "type": "User"}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/graphql"):
			if m.fixedIssueClosedBy == 0 {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": map[string]any{"nodes": []map[string]any{{"number": m.fixedIssueClosedBy, "url": "https://github.com/testorg/testrepo/pull/" + strconv.Itoa(m.fixedIssueClosedBy), "merged": true, "mergedAt": "2026-10-28T11:00:00Z"}}}}}}})
		case r.Method == http.MethodGet && m.fixedIssueClosedBy > 0 && strings.HasSuffix(path, "/pulls/"+strconv.Itoa(m.fixedIssueClosedBy)):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": m.fixedIssueClosedBy, "state": "closed", "merged": true, "merged_at": "2026-10-28T11:00:00Z", "merge_commit_sha": "25de3ba24fa0c0ffee0000000000000000000000", "user": map[string]any{"login": "hive[bot]", "type": "Bot"}, "head": map[string]any{"sha": "cafecafecafecafecafecafecafecafecafecafe"}, "base": map[string]any{"ref": "main"}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/90"):
			if m.supersederState == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 90, "state": m.supersederState, "merged": m.supersederMerged, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}, "head": map[string]any{"sha": "beefbeefbeefbeefbeefbeefbeefbeefbeefbeef"}})
		case r.Method == http.MethodGet && (strings.HasSuffix(path, "/pulls/8") || strings.HasSuffix(path, "/pulls/9")):
			// PR #9 is a second hive PR nested under issue #7 (the per-PR
			// delegated decision); it reads like #8.
			n := 8
			if strings.HasSuffix(path, "/pulls/9") {
				n = 9
			}
			author := map[string]any{"login": "hive[bot]", "type": "Bot"}
			if m.prForeign {
				author = map[string]any{"login": "someone", "type": "User"}
			}
			mergeState := m.prMergeState
			if mergeState == "" {
				mergeState = "clean"
			}
			prLabels := make([]map[string]string, 0, len(m.prLabels))
			for _, label := range m.prLabels {
				prLabels = append(prLabels, map[string]string{"name": label})
			}
			headRepo := "testorg/testrepo"
			if m.prFork {
				headRepo = "someone/testrepo"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "open", "body": m.prBody, "labels": prLabels, "user": author, "mergeable": mergeState != "dirty", "mergeable_state": mergeState, "maintainer_can_modify": !m.prNoMaintainerEdits,
				"head": map[string]any{"sha": "feedfacefeedfacefeedfacefeedfacefeedface", "repo": map[string]any{"full_name": headRepo}},
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "testorg/testrepo"}}})
		case r.Method == http.MethodGet && strings.Contains(path, "/compare/"):
			files := []map[string]string{}
			if m.workflowPath != "" {
				files = append(files, map[string]string{"filename": m.workflowPath})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/pulls/8/update-branch"):
			m.updates++
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"message":"Updating pull request branch."}`)
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/pulls/8/merge"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.merges = append(m.merges, body)
			_, _ = io.WriteString(w, `{"sha":"0ddba11","merged":true,"message":"Pull Request successfully merged"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/8/reviews"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls/8/reviews"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.reviews = append(m.reviews, body)
			_, _ = io.WriteString(w, `{"id":1,"state":"APPROVED"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/commits/feedfacefeedfacefeedfacefeedfacefeedface/status"):
			switch {
			case m.prRedCI:
				_, _ = io.WriteString(w, `{"state":"failure","statuses":[{"context":"ci/test","state":"failure"}]}`)
			case m.prNoCI:
				_, _ = io.WriteString(w, `{"state":"pending","statuses":[]}`)
			case m.prTidePending:
				_, _ = io.WriteString(w, `{"state":"pending","statuses":[{"context":"ci/test","state":"success"},{"context":"tide","state":"pending"}]}`)
			default:
				_, _ = io.WriteString(w, `{"state":"success","statuses":[{"context":"ci/test","state":"success"}]}`)
			}
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/commits/feedfacefeedfacefeedfacefeedfacefeedface/check-runs"):
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		case r.Method == http.MethodGet && strings.Contains(path, "/commits/") && !strings.HasSuffix(path, "/status") && !strings.HasSuffix(path, "/check-runs"):
			sha := path[strings.LastIndex(path, "/")+1:]
			login, typ := "someone", "User"
			if by, ok := m.commitBy[sha]; ok {
				login = by
				if strings.HasSuffix(by, "[bot]") {
					typ = "Bot"
				}
			}
			who := map[string]any{"login": login, "type": typ}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": who, "committer": who, "commit": map[string]any{"author": map[string]any{"name": login, "email": login + "@example.com"}, "committer": map[string]any{"name": login, "email": login + "@example.com"}}})
		case r.Method == http.MethodGet && strings.Contains(path, "/timeline"):
			if m.timeline == nil {
				_, _ = io.WriteString(w, `[]`)
			} else {
				_ = json.NewEncoder(w).Encode(m.timeline)
			}
		case r.Method == http.MethodGet && (strings.HasSuffix(path, "/pulls/8/commits") || strings.HasSuffix(path, "/pulls/8/reviews")):
			// No author activity: the contributor reads as offline.
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/comments"):
			prior := make([]map[string]any, 0, len(m.priorComments))
			for i, body := range m.priorComments {
				prior = append(prior, map[string]any{"id": i + 1, "body": body, "created_at": "2026-10-28T12:01:00Z"})
			}
			_ = json.NewEncoder(w).Encode(prior)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			var c map[string]string
			_ = json.NewDecoder(r.Body).Decode(&c)
			m.comments = append(m.comments, c["body"])
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodGet && strings.Contains(path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.Contains(path, "/issues/") && strings.HasSuffix(path, "/labels"):
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			m.added = append(m.added, labels...)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete && strings.Contains(path, "/labels/"):
			m.removed = append(m.removed, path[strings.LastIndex(path, "/")+1:])
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.Contains(path, "/issues/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.edits = append(m.edits, body)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected GitHub API request: %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghSrv.Close)
	srv := newFullServer(t)
	srv.deps.Config.HiveID = "h1"
	srv.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, slog.Default(), ghSrv.URL)
	srv.deps.GHClient.SetAppBotLogin("hive[bot]")
	// The PR-detail cache is process-global and keyed by repo+number, so a PR
	// #8 cached by another test (with a different author or labels) would
	// answer GetPRAuthor/GetPRState here instead of this mock. A zero TTL
	// turns cache reads off for this client (hivecommons/hive#15461).
	srv.deps.GHClient.SetPRDetailTTLFunc(func() time.Duration { return 0 })
	store := review.NewHumanChangeRequests("")
	prev := repoItemDecisionChangeRequests
	repoItemDecisionChangeRequests = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { repoItemDecisionChangeRequests = prev })
	return srv
}

func postRepoItemDecision(t *testing.T, srv *Server, number int, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/"+strconv.Itoa(number)+"/decision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func decisionAuditActions(srv *Server) []string {
	var out []string
	for _, e := range srv.GetAudit().Recent(20) {
		out = append(out, e.Action)
	}
	return out
}

func TestRepoItemDecisionAuthorizationAndValidation(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)

	if w := postRepoItemDecision(t, srv, 7, "", `{"decision":"approve"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d want 401", w.Code)
	}
	if w := postRepoItemDecision(t, srv, 7, "reader", `{"decision":"approve"}`); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d want 403", w.Code)
	}
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"merge"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown verb: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
	if len(mock.added)+len(mock.comments)+len(mock.edits) != 0 {
		t.Fatalf("refused requests must write nothing: added=%v comments=%v edits=%v", mock.added, mock.comments, mock.edits)
	}
}

// TestRepoItemApproveResultWording (owner, hivecommons/dibs#510 — "that's
// a confusing response"): the approve toast leads with the outcome in
// plain words, says who merges next, flags the sentinel alert only while
// it is still open, and lists what was posted as a secondary line.
func TestRepoItemApproveResultWording(t *testing.T) {
	cases := []struct {
		name      string
		facts     repoItemApproveFacts
		outcome   string
		next      string
		posted    string
		cleared   bool
		stillOpen bool
	}{
		{
			name:    "sentinel released, lane merges",
			facts:   repoItemApproveFacts{User: "clubanderson", Released: []string{"approval-recorded", "sentinel-label", "sentinel-acknowledged", "admitted"}, SentinelLabel: "sentinel-alert", SoakMinutes: 30},
			outcome: "Approved #510 as @clubanderson · sentinel alert cleared by your review.",
			next:    "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked (≈30 min).",
			posted:  "`/lgtm` posted|`/approve` posted|`sentinel-alert` label removed|head admitted as-is on your approval",
			cleared: true,
		},
		{
			name:      "sentinel still open",
			facts:     repoItemApproveFacts{User: "clubanderson", Released: []string{"approval-recorded"}, SentinelOpen: true, SentinelLabel: "sentinel-alert", SoakMinutes: 30},
			outcome:   "Approved #510 as @clubanderson · sentinel alert still open.",
			next:      "The `sentinel-alert` label is still on the PR: a maintainer must review the flagged head and take the label off on GitHub before hive's merge sweep will touch it.",
			posted:    "`/lgtm` posted|`/approve` posted",
			stillOpen: true,
		},
		{
			name:    "human must merge",
			facts:   repoItemApproveFacts{User: "clubanderson", Released: []string{"approval-recorded", "needs-human"}, Stage: stagelabels.ReadyForHumanMerge.Label(), SoakMinutes: 3},
			outcome: "Approved #510 as @clubanderson.",
			next:    "Hive cannot merge this one (trust or role blocks it): use Approve and let hive merge, merge it now as maintainer, or merge it on GitHub yourself.",
			posted:  "`/lgtm` posted|`/approve` posted|`needs-human` label removed",
		},
		{
			name:    "prow-managed repo: approval recorded, not claimed approved for prow",
			facts:   repoItemApproveFacts{User: "clubanderson", Released: []string{"approval-recorded"}, SoakMinutes: 3, Prow: true},
			outcome: "Approval of #510 recorded in hive as @clubanderson.",
			next:    "Prow approval still needs an OWNERS reviewer: the relayed `/approve` came from hive's account, which Prow does not count. Hive's merge sweep merges it on its next pass once CI is green and the head has soaked (≈3 min).",
			posted:  "`/lgtm` posted|`/approve` posted",
		},
		{
			name:    "no user, no soak figure",
			facts:   repoItemApproveFacts{Released: []string{"approval-recorded"}},
			outcome: "Approved #510.",
			next:    "Hive's merge sweep merges it on its next pass once CI is green and the head has soaked.",
			posted:  "`/lgtm` posted|`/approve` posted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repoItemDecisionResultWith("hivecommons/dibs", 510, "pr", repoItemDecisionApprove, "", "", "", tc.facts)
			if got.Action != rowActionApproved || got.Outcome != tc.outcome || got.Next != tc.next {
				t.Fatalf("result = %+v", got)
			}
			if p := strings.Join(got.Posted, "|"); p != tc.posted {
				t.Fatalf("posted = %q", p)
			}
			if got.SentinelCleared != tc.cleared || got.SentinelOpen != tc.stillOpen {
				t.Fatalf("sentinel flags cleared=%v open=%v", got.SentinelCleared, got.SentinelOpen)
			}
			for _, bad := range []string{"released:", "merges nothing", "merge gate takes it"} {
				if strings.Contains(got.Outcome+got.Next, bad) {
					t.Fatalf("old wording %q survives: %+v", bad, got)
				}
			}
			if !tc.facts.Prow && strings.Contains(got.Outcome+got.Next, "Prow") {
				t.Fatalf("non-prow repo mentions Prow: %+v", got)
			}
		})
	}
	// The legacy builder keeps the same shape for callers without facts.
	if got := repoItemDecisionResult("o/r", 1, "pr", repoItemDecisionApprove, "", "", "", nil); got.Outcome != "Approved #1." {
		t.Fatalf("legacy = %+v", got)
	}
}
