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

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
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
	// prForeign makes PR #8 look like a human opened it.
	prForeign bool
	// prMergeState is PR #8's mergeable_state ("" = clean); prRedCI and
	// prNoCI shape the head's combined status when the state alone does not
	// settle CI (e.g. "blocked").
	prMergeState string
	prRedCI      bool
	prNoCI       bool
}

func newRepoItemDecisionServer(t *testing.T, m *repoItemDecisionMock) *Server {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/collaborators/writer/permission"):
			_, _ = io.WriteString(w, `{"permission":"write"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/collaborators/reader/permission"):
			_, _ = io.WriteString(w, `{"permission":"read"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues/7"):
			labels := make([]map[string]string, 0, len(m.issueLabels))
			for _, label := range m.issueLabels {
				labels = append(labels, map[string]string{"name": label})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": labels, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues/8"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "state": "open", "labels": []any{}, "pull_request": map[string]any{"url": "x"}, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/8"):
			author := map[string]any{"login": "hive[bot]", "type": "Bot"}
			if m.prForeign {
				author = map[string]any{"login": "someone", "type": "User"}
			}
			mergeState := m.prMergeState
			if mergeState == "" {
				mergeState = "clean"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "state": "open", "labels": []any{}, "user": author, "mergeable": true, "mergeable_state": mergeState, "head": map[string]any{"sha": "feedfacefeedfacefeedfacefeedfacefeedface"}})
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
		case r.Method == http.MethodGet && strings.Contains(path, "/timeline"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
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
