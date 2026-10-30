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

	"github.com/hivecommons/hive/pkg/config"
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

func TestRepoItemDecisionApproveUnparksAndAudits(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.Status != "unparked" || resp.Label != github.HumanAckLabel {
		t.Fatalf("response = %+v", resp)
	}
	// approved-direction is applied and the stage engine re-derives the
	// stage off the park; nothing else may be written.
	if len(mock.added) == 0 || mock.added[0] != github.HumanAckLabel {
		t.Fatalf("added = %v, want %s first", mock.added, github.HumanAckLabel)
	}
	for _, l := range mock.added[1:] {
		if !strings.HasPrefix(l, "stage/") {
			t.Fatalf("unexpected label written: %q (all: %v)", l, mock.added)
		}
	}
	if len(mock.edits) != 0 {
		t.Fatalf("approve must not close anything, got edits %v", mock.edits)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Un-parked by @writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionApprove) {
		t.Fatalf("audit missing %s: %v", auditRepoItemDecisionApprove, decisionAuditActions(srv))
	}
}

func TestRepoItemDecisionDirectionRequiresTextAndParkedIssue(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)

	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"direction"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("direction without text: code=%d body=%s", w.Code, w.Body.String())
	}
	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"direction","comment":"ship option B"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("direction: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Decision: ship option B") {
		t.Fatalf("comments = %v", mock.comments)
	}

	mock.issueLabels = []string{"bug"}
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"approve"}`); w.Code != http.StatusConflict {
		t.Fatalf("approve on un-parked issue: code=%d want 409 body=%s", w.Code, w.Body.String())
	}
	// A PR sent as an issue is told to resend as type "pr" rather than
	// being run through the issue un-park.
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"approve"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("approve on PR typed as issue: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemDecisionCloseWontFixCommentsAndCloses(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-wontfix","comment":"out of scope for v5"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "won't fix by @writer") || !strings.Contains(mock.comments[0], "out of scope for v5") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonNotPlanned {
		t.Fatalf("edits = %v", mock.edits)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionClose) {
		t.Fatalf("audit missing %s: %v", auditRepoItemDecisionClose, decisionAuditActions(srv))
	}
}

// close-read (hivecommons/hive#15381): an advisory issue is closed as
// completed through the same comment + close path; a PR is refused.
func TestRepoItemDecisionCloseReadClosesAsCompleted(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"triage/advisory"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Closed as read (advisory acknowledged) by @writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonCompleted {
		t.Fatalf("edits = %v", mock.edits)
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "closed" || resp.Result == nil || resp.Result.Outcome != "Closed as read (advisory acknowledged)." {
		t.Fatalf("resp = %+v", resp)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-read","type":"pr"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("close-read on a PR: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
}

func TestRepoItemDecisionCloseSupersededPRUsesSweepPath(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close superseded: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Label != github.SupersededLabel {
		t.Fatalf("label = %q, want %s", resp.Label, github.SupersededLabel)
	}
	if strings.Join(mock.added, ",") != github.SupersededLabel {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" {
		t.Fatalf("edits = %v", mock.edits)
	}
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

func TestRepoItemDecisionApprovePRPostsLgtmAndApproveComments(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"approve","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve PR: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "approval-commented" {
		t.Fatalf("status = %q", resp.Status)
	}
	if len(mock.comments) != 2 || !strings.HasPrefix(mock.comments[0], "/lgtm\n") || !strings.HasPrefix(mock.comments[1], "/approve\n") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if !strings.Contains(mock.comments[0], "@writer") {
		t.Fatalf("command comment must name the operator: %q", mock.comments[0])
	}
	if len(mock.added)+len(mock.edits) != 0 {
		t.Fatalf("approval must not label or edit the PR (no merge): added=%v edits=%v", mock.added, mock.edits)
	}
	if actions := decisionAuditActions(srv); len(actions) == 0 || actions[0] != auditRepoItemDecisionApprove {
		t.Fatalf("audit = %v", actions)
	}
}

// TestRepoItemDecisionApprovePRReleasesThePark: the owner's Approve on a
// parked PR is the human review it waited for — the approval is recorded
// against the head (the lanes and the stage engine read it as the answer to
// requires_human / decision-requested), `needs-human` and the sentinel label
// come off, and the tracker cache is dropped so the next read re-derives the
// stage (hivecommons/hive#14821, #14822, crux#30 stayed at 085 with only the
// command comments written).
func TestRepoItemDecisionApprovePRReleasesThePark(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human", "sentinel-alert", "role/trusted"}}
	srv := newRepoItemDecisionServer(t, mock)
	// Own store: with no stage record there is no head admission to
	// report (hivecommons/hive#15136), whatever other tests left in the
	// process-wide store.
	srv.stageStore = stagelabels.NewStore("", time.Now)
	srv.stageTrackerCacheMu.Lock()
	gen := srv.stageTrackerGen
	srv.stageTrackerCacheMu.Unlock()

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"approve","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve PR: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "approval-commented" || strings.Join(resp.Released, ",") != "approval-recorded,needs-human,sentinel-label" {
		t.Fatalf("response = %+v", resp)
	}
	req, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, "feedfacefeedfacefeedfacefeedfacefeedface")
	if !ok || !req.IsApproval() || req.Actor != "writer" {
		t.Fatalf("approval must stand against the head: %+v ok=%v", req, ok)
	}
	if strings.Join(mock.removed, ",") != "needs-human,085-needs-decision,sentinel-alert" {
		t.Fatalf("removed = %v", mock.removed)
	}
	if len(mock.comments) != 2 || len(mock.added)+len(mock.edits) != 0 {
		t.Fatalf("approval still posts the two commands and nothing else: comments=%d added=%v edits=%v", len(mock.comments), mock.added, mock.edits)
	}
	srv.stageTrackerCacheMu.Lock()
	stale, after := srv.stageTrackerCache.stale, srv.stageTrackerGen
	srv.stageTrackerCacheMu.Unlock()
	if !stale || after == gen {
		t.Fatalf("tracker cache must be invalidated: stale=%v gen %d -> %d", stale, gen, after)
	}
	detail := srv.GetAudit().Recent(1)[0].Detail
	if !strings.Contains(detail, "released=approval-recorded,needs-human,sentinel-label") {
		t.Fatalf("audit detail = %q", detail)
	}
}

func TestRepoItemDecisionDirectionPRRecordsChangeRequest(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"direction","type":"pr","comment":"drop the retry loop"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("direction PR: code=%d body=%s", w.Code, w.Body.String())
	}
	req, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, "feedfacefeedfacefeedfacefeedfacefeedface")
	if !ok || req.Body != "drop the retry loop" || req.Actor != "writer" {
		t.Fatalf("stored request = %+v ok=%v", req, ok)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Decision recorded by @writer") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if actions := decisionAuditActions(srv); len(actions) == 0 || actions[0] != auditRepoItemDecisionDirection {
		t.Fatalf("audit = %v", actions)
	}

	mock.prForeign = true
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"direction","type":"pr","comment":"x"}`); w.Code != http.StatusConflict {
		t.Fatalf("foreign PR direction: code=%d want 409 body=%s", w.Code, w.Body.String())
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"direction","type":"pr"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("direction without text: code=%d want 400", w.Code)
	}
}

func TestRepoItemDecisionCloseSupersededByNamesTheWinner(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"#458"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close superseded by: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "superseded by testorg/testrepo#458 by @writer") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"#8"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("self supersession: code=%d want 400", w.Code)
	}
	if w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"close-superseded","type":"pr","superseded_by":"nope"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("garbage ref: code=%d want 400", w.Code)
	}
}

func TestParseRepoItemSupersededBy(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"458":             "o/r#458",
		" #458 ":          "o/r#458",
		"acme/console#12": "acme/console#12",
		"https://github.com/acme/console/pull/12":     "acme/console#12",
		"https://github.com/acme/console/issues/12/":  "acme/console#12",
		"https://ghe.example.com/acme/console/pull/3": "acme/console#3",
	}
	for in, want := range cases {
		got, err := parseRepoItemSupersededBy(in, "o/r")
		if err != nil || got != want {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "#0", "r#4", "https://github.com/acme/console", "a/b/c#4"} {
		if _, err := parseRepoItemSupersededBy(bad, "o/r"); err == nil {
			t.Errorf("parse(%q) must fail", bad)
		}
	}
}

func postRepoItemDecisionAs(t *testing.T, srv *Server, number int, user, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/"+strconv.Itoa(number)+"/decision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", user)
	req.Header.Set("X-Hive-Role", role)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// TestRepoItemDecisionMergeNowMergesAsMaintainer: "Approve & merge as
// maintainer" on a Ready-for-human-merge PR leaves an attributed note, then
// merges through the hive's App against the head the operator saw, audits
// it on the dashboard path and answers the shared result (merged)
// (hivecommons/hive#15078).
func TestRepoItemDecisionMergeNowMergesAsMaintainer(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr","comment":"release notes look right"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("merge-now: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "merged" || resp.Head != "feedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionMerged || !strings.Contains(resp.Result.Outcome, "`main`") || !strings.Contains(resp.Result.Outcome, "attributed to you") {
		t.Fatalf("result = %+v", resp.Result)
	}
	if len(mock.merges) != 1 || mock.merges[0]["sha"] != "feedfacefeedfacefeedfacefeedfacefeedface" {
		t.Fatalf("merge must pin the head the operator saw: %v", mock.merges)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Merged as maintainer by @writer") || !strings.Contains(mock.comments[0], "> release notes look right") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if actions := decisionAuditActions(srv); len(actions) == 0 || actions[0] != auditRepoItemDecisionMerge {
		t.Fatalf("audit = %v", actions)
	}
}

// TestRepoItemDecisionQueueMergeApprovesForTheSweep: "Approve — let hive
// merge" posts the App's approval naming the operator as the queuer and
// applies the auto-merge label; the dashboard merges nothing.
func TestRepoItemDecisionQueueMergeApprovesForTheSweep(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"queue-merge","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("queue-merge: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "queued-for-merge" || resp.Label != srv.deps.GHClient.AutoMergeLabel() {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionQueued || !strings.Contains(resp.Result.Next, "merge sweep") {
		t.Fatalf("result = %+v", resp.Result)
	}
	if len(mock.merges) != 0 {
		t.Fatalf("queue-merge must not merge: %v", mock.merges)
	}
	if len(mock.reviews) != 1 || mock.reviews[0]["event"] != "APPROVE" || !strings.HasPrefix(mock.reviews[0]["body"].(string), "Approved by @writer for Hive auto-merge on green CI.") {
		t.Fatalf("reviews = %v", mock.reviews)
	}
	if !strings.Contains(mock.reviews[0]["body"].(string), "Approved from the Hive dashboard by @writer") {
		t.Fatalf("approval must carry the dashboard note: %v", mock.reviews[0]["body"])
	}
	if len(mock.added) != 1 || mock.added[0] != srv.deps.GHClient.AutoMergeLabel() {
		t.Fatalf("labels added = %v", mock.added)
	}
	if actions := decisionAuditActions(srv); len(actions) == 0 || actions[0] != auditRepoItemDecisionQueue {
		t.Fatalf("audit = %v", actions)
	}
}

// TestRepoItemDecisionMergeDecisionsGuard: both merge answers need the
// merger or owner role on top of write access, apply to PRs only, and
// refuse the operator's own PR.
func TestRepoItemDecisionMergeDecisionsGuard(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newRepoItemDecisionServer(t, mock)
	for _, decision := range []string{"merge-now", "queue-merge"} {
		if w := postRepoItemDecisionAs(t, srv, 8, "writer", "write", `{"decision":"`+decision+`","type":"pr"}`); w.Code != http.StatusForbidden {
			t.Errorf("%s as a plain writer: code=%d body=%s", decision, w.Code, w.Body.String())
		}
		if w := postRepoItemDecisionAs(t, srv, 7, "writer", "merger", `{"decision":"`+decision+`","type":"issue"}`); w.Code != http.StatusBadRequest {
			t.Errorf("%s on an issue: code=%d body=%s", decision, w.Code, w.Body.String())
		}
		if w := postRepoItemDecisionAs(t, srv, 8, "someone", "merger", `{"decision":"`+decision+`","type":"pr"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "you opened this pull request") {
			t.Errorf("%s on one's own PR: code=%d body=%s", decision, w.Code, w.Body.String())
		}
	}
	if len(mock.merges)+len(mock.reviews) != 0 {
		t.Fatalf("guards must stop before GitHub writes: merges=%v reviews=%v", mock.merges, mock.reviews)
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
			next:    "Hive cannot merge this one (trust or role blocks it): merge it on GitHub yourself, or use Approve & merge as maintainer.",
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

// The real hivecommons/hive#15381 shape (a dedupe-checklist advisory: the
// triage/advisory label, a "Decision required: no" body, bot author) closes
// as completed with the acknowledgement comment; on a paused repo the same
// request is a 200 refused result whose Outcome says why, and nothing is
// written (hivecommons/hive#15436).
func TestRepoItemDecisionCloseReadAdvisoryFixtureAndPausedRefusal(t *testing.T) {
	mock := &repoItemDecisionMock{
		issueLabels: []string{"help wanted", "agent/dedupe", "triage/advisory"},
		issueTitle:  "triage: duplicate and overlapping open issues/PRs to consolidate",
		issueBody:   "<!-- hive:dedupe-checklist repo=hivecommons/hive -->\nDecision required: no\n\n## Summary\n\nTriage pass over the 28 open issues and 12 open PRs. Found 0 near-duplicate pairs.",
	}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read","type":"issue"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "closed" || resp.Result == nil || resp.Result.Action != rowActionClosed {
		t.Fatalf("resp = %+v", resp)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "Closed as read (advisory acknowledged) by @writer") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonCompleted {
		t.Fatalf("edits = %v", mock.edits)
	}

	// Paused repo: refused with a reason, nothing posted.
	mock.comments, mock.edits = nil, nil
	srv.deps.Config.Project.PausedRepos = []config.RepoPause{{Repo: "testrepo", By: "clubanderson", Reason: "release freeze"}}
	t.Cleanup(func() { srv.deps.Config.Project.PausedRepos = nil })
	w = postRepoItemDecision(t, srv, 7, "writer", `{"decision":"close-read","type":"issue"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("paused close-read: code=%d body=%s", w.Code, w.Body.String())
	}
	resp = repoItemDecisionResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != rowActionRefused || resp.Result == nil || resp.Result.Action != rowActionRefused {
		t.Fatalf("paused resp = %+v", resp)
	}
	if !strings.HasPrefix(resp.Result.Outcome, "Close refused: repo paused") || !strings.Contains(resp.Result.Outcome, "release freeze") {
		t.Fatalf("outcome = %q, want the pause and its reason", resp.Result.Outcome)
	}
	if len(mock.comments)+len(mock.edits) != 0 {
		t.Fatalf("a refused close must write nothing: comments=%v edits=%v", mock.comments, mock.edits)
	}
}
