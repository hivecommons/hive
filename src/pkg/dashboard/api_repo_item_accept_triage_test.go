package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The activity monitor's "Accept triage" row action: the one-click form of a
// maintainer accepting triage on GitHub — add triage/accepted, remove
// needs-triage (and a Hive-written stage/015 label) — behind the Hold
// authorisation, attributed to the dashboard user, idempotent, issues only.

type acceptTriageMock struct {
	permission     string
	issueLabels    []string
	isPullRequest  bool
	issueMissing   bool
	added, removed []string
	created        []string
}

func newAcceptTriageServer(t *testing.T, m *acceptTriageMock) *Server {
	t.Helper()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/writer/permission"):
			_, _ = io.WriteString(w, `{"permission":"`+m.permission+`"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/reader/permission"):
			_, _ = io.WriteString(w, `{"permission":"read"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7"):
			if m.issueMissing {
				http.NotFound(w, r)
				return
			}
			labels := make([]map[string]string, 0, len(m.issueLabels))
			for _, label := range m.issueLabels {
				labels = append(labels, map[string]string{"name": label})
			}
			body := map[string]any{"number": 7, "state": "open", "labels": labels}
			if m.isPullRequest {
				body["pull_request"] = map[string]any{"url": "https://api.github.com/repos/testorg/testrepo/pulls/7"}
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/labels"):
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			m.added = append(m.added, labels...)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var label map[string]any
			_ = json.NewDecoder(r.Body).Decode(&label)
			if name, _ := label["name"].(string); name != "" {
				m.created = append(m.created, name)
			}
			_ = json.NewEncoder(w).Encode(label)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			part := strings.TrimPrefix(r.URL.EscapedPath(), "/repos/testorg/testrepo/issues/7/labels/")
			first, _ := url.PathUnescape(part)
			second, _ := url.PathUnescape(first)
			m.removed = append(m.removed, second)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected GitHub API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghSrv.Close)
	srv := newFullServer(t)
	srv.deps.Config.HiveID = "h1"
	srv.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, slog.Default(), ghSrv.URL)
	return srv
}

func postAcceptTriage(t *testing.T, srv *Server, user string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/7/accept-triage", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if owner {
		markOwnerRequest(req)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestRepoItemAcceptTriageAuthorizationAndLabels(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", issueLabels: []string{"bug", "needs-triage", "stage/015-triage-needed"}}
	srv := newAcceptTriageServer(t, mock)

	if w := postAcceptTriage(t, srv, "reader", false); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if w := postAcceptTriage(t, srv, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d want 401 body=%s", w.Code, w.Body.String())
	}
	if len(mock.added)+len(mock.removed) != 0 {
		t.Fatalf("refused requests wrote labels: added=%v removed=%v", mock.added, mock.removed)
	}

	w := postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("writer: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoAcceptTriageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.AlreadyAccepted || resp.AcceptLabel != "triage/accepted" {
		t.Fatalf("response = %+v", resp)
	}
	if got := strings.Join(mock.added, ","); got != "triage/accepted" {
		t.Fatalf("added = %q, want triage/accepted", got)
	}
	if got := strings.Join(mock.created, ","); got != "triage/accepted" {
		t.Fatalf("created = %q, want the accept label ensured", got)
	}
	if got := strings.Join(mock.removed, ","); got != "needs-triage,stage/015-triage-needed" {
		t.Fatalf("removed = %q, want needs-triage and the 015 stage label", got)
	}
	if strings.Join(resp.Removed, ",") != "needs-triage,stage/015-triage-needed" || strings.Join(resp.Added, ",") != "triage/accepted" {
		t.Fatalf("response labels = %+v", resp)
	}

	var audited bool
	for _, entry := range srv.GetAudit().Recent(10) {
		if entry.Action == acceptTriageAuditAction {
			audited = true
			if entry.User != "writer" {
				t.Fatalf("audit user = %q, want the dashboard user", entry.User)
			}
			if !strings.Contains(entry.Detail, "triage/accepted") || !strings.Contains(entry.Detail, "needs-triage") {
				t.Fatalf("audit detail = %q", entry.Detail)
			}
		}
	}
	if !audited {
		t.Fatal("no audit entry for the accepted triage")
	}

	if resp.Result == nil || resp.Result.Action != rowActionTriaged || resp.Result.Next != acceptTriageNextText("") {
		t.Fatalf("result without a governor kick = %+v", resp.Result)
	}

	// The verified owner role passes without a repo permission lookup.
	mock.added, mock.removed, mock.created = nil, nil, nil
	if w := postAcceptTriage(t, srv, "", true); w.Code != http.StatusOK {
		t.Fatalf("owner: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(mock.added, ",") != "triage/accepted" {
		t.Fatalf("owner added = %v", mock.added)
	}
}

func TestRepoItemAcceptTriageIdempotentAndIssuesOnly(t *testing.T) {
	mock := &acceptTriageMock{permission: "maintain", issueLabels: []string{"bug", "triage/accepted"}}
	srv := newAcceptTriageServer(t, mock)

	w := postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("already accepted: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoAcceptTriageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.AlreadyAccepted || len(mock.added)+len(mock.removed)+len(mock.created) != 0 {
		t.Fatalf("already accepted must be a no-op: resp=%+v added=%v removed=%v created=%v", resp, mock.added, mock.removed, mock.created)
	}
	if resp.Result == nil || resp.Result.Action != rowActionNoop || !strings.HasPrefix(resp.Result.Outcome, "No change — ") {
		t.Fatalf("no-op result = %+v", resp.Result)
	}
	for _, entry := range srv.GetAudit().Recent(10) {
		if entry.Action == acceptTriageAuditAction {
			t.Fatal("a no-op must not be audited as a write")
		}
	}

	// Accepted label on, but the awaiting label lingers: only the removal happens.
	mock.issueLabels = []string{"triage/accepted", "needs-triage"}
	w = postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("lingering awaiting: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.added) != 0 || strings.Join(mock.removed, ",") != "needs-triage" {
		t.Fatalf("lingering awaiting: added=%v removed=%v", mock.added, mock.removed)
	}
	resp = repoAcceptTriageResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Result == nil || resp.Result.Action != rowActionTriaged || resp.Result.Target.Kind != rowActionTargetIssue {
		t.Fatalf("result = %+v", resp.Result)
	}

	// A pull request has no triage state.
	mock.isPullRequest = true
	mock.added, mock.removed = nil, nil
	mock.issueLabels = []string{"needs-triage"}
	if w := postAcceptTriage(t, srv, "writer", false); w.Code != http.StatusNotFound {
		t.Fatalf("pull request: code=%d want 404 body=%s", w.Code, w.Body.String())
	}
	if len(mock.added)+len(mock.removed) != 0 {
		t.Fatalf("pull request must not be labelled: added=%v removed=%v", mock.added, mock.removed)
	}

	// An unknown item is 404 too.
	mock.isPullRequest, mock.issueMissing = false, true
	if w := postAcceptTriage(t, srv, "writer", false); w.Code != http.StatusNotFound {
		t.Fatalf("missing: code=%d want 404 body=%s", w.Code, w.Body.String())
	}

	// Bad paths and a missing GitHub client.
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/0/accept-triage", strings.NewReader(`{}`))
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("item 0: code=%d want 400", rec.Code)
	}
	srv.deps.GHClient = nil
	if w := postAcceptTriage(t, srv, "", true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client: code=%d want 503", w.Code)
	}
}

// The configured reporter-trust labels are honoured: a custom accept label
// and awaiting label, with the default needs-triage still swept.
func TestRepoItemAcceptTriageUsesConfiguredLabels(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", issueLabels: []string{"awaiting-maintainer", "needs-triage"}}
	srv := newAcceptTriageServer(t, mock)
	awaiting := "awaiting-maintainer"
	srv.deps.Config.Project.IssueFilter.ReporterTrust = config.ReporterTrustConfig{
		UntrustedRequireLabels: []string{"triage/ok", "triage/accepted"},
		AwaitingLabel:          &awaiting,
	}
	w := postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(mock.added, ",") != "triage/ok" {
		t.Fatalf("added = %v, want the first configured accept label", mock.added)
	}
	if strings.Join(mock.removed, ",") != "awaiting-maintainer,needs-triage" {
		t.Fatalf("removed = %v", mock.removed)
	}
}

// Pure helpers and the status/tracker patch: a 015 record on an issue whose
// labels already say triage/accepted reads 010 Actionable until the engine
// re-derives it; the snapshot's labels are patched so the next reconcile
// agrees.
func TestRepoItemAcceptTriagePlanAndStalenessRule(t *testing.T) {
	accept, awaiting := "triage/accepted", []string{"needs-triage"}
	add, remove := acceptTriagePlan([]string{"bug", "Needs-Triage", "stage/015-triage-needed"}, accept, awaiting)
	if strings.Join(add, ",") != "triage/accepted" || strings.Join(remove, ",") != "Needs-Triage,stage/015-triage-needed" {
		t.Fatalf("plan add=%v remove=%v", add, remove)
	}
	if add, remove := acceptTriagePlan([]string{"triage/accepted"}, accept, awaiting); len(add)+len(remove) != 0 {
		t.Fatalf("accepted plan add=%v remove=%v", add, remove)
	}
	after := acceptTriageLabelsAfter([]string{"bug", "needs-triage"}, []string{"triage/accepted"}, []string{"needs-triage"})
	if strings.Join(after, ",") != "bug,triage/accepted" {
		t.Fatalf("after = %v", after)
	}

	srv := newFullServer(t)
	rec := stagelabels.Record{Stage: stagelabels.TriageNeeded, EnteredAt: time.Now()}
	if !srv.stageTrackerAcceptedTriageRecord(rec, []string{"triage/accepted"}) {
		t.Fatal("accepted labels must retire a 015 record")
	}
	if srv.stageTrackerAcceptedTriageRecord(rec, []string{"triage/accepted", "needs-triage"}) {
		t.Fatal("an awaiting label still present keeps 015")
	}
	if srv.stageTrackerAcceptedTriageRecord(rec, []string{"bug"}) {
		t.Fatal("no accept label: not accepted")
	}
	if srv.stageTrackerAcceptedTriageRecord(stagelabels.Record{Stage: stagelabels.Claimed}, []string{"triage/accepted"}) {
		t.Fatal("only a 015 record is retired")
	}

	srv.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "testrepo", Full: "testorg/testrepo",
		ActionableIssues:    []any{github.Issue{Number: 7, Labels: []string{"bug", "needs-triage"}}, github.Issue{Number: 8, Labels: []string{"needs-triage"}}},
		NonActionableIssues: []any{github.RepoWorkIssue{Number: 7, Labels: []string{"needs-triage"}}},
	}}}
	before := srv.statusSeq
	minSeq := srv.applyAcceptTriageToStatus("testorg/testrepo", 7, []string{"triage/accepted"}, []string{"needs-triage"})
	if minSeq != before+1 || srv.statusSeq != before+1 {
		t.Fatalf("statusSeq = %d minSeq=%d before=%d", srv.statusSeq, minSeq, before)
	}
	repo := srv.status.Repos[0]
	if got := strings.Join(repo.ActionableIssues[0].(github.Issue).Labels, ","); got != "bug,triage/accepted" {
		t.Fatalf("actionable #7 labels = %q", got)
	}
	if got := strings.Join(repo.ActionableIssues[1].(github.Issue).Labels, ","); got != "needs-triage" {
		t.Fatalf("actionable #8 must be untouched, got %q", got)
	}
	if got := strings.Join(repo.NonActionableIssues[0].(github.RepoWorkIssue).Labels, ","); got != "triage/accepted" {
		t.Fatalf("non-actionable #7 labels = %q", got)
	}
}

// The result's "next" line carries the governor's next pass when one is
// published, so the toast reads "≈13m" (hivecommons/hive#15215).
func TestRepoItemAcceptTriageNextPassETA(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	srv := newAcceptTriageServer(t, &acceptTriageMock{permission: "write"})
	if got := srv.governorNextPassIn(now); got != "" {
		t.Fatalf("no status: eta = %q, want none", got)
	}
	srv.statusMu.Lock()
	srv.status = &StatusPayload{Governor: FrontendGovernor{NextKickAt: now.Add(13 * time.Minute).Format(time.RFC3339)}}
	srv.statusMu.Unlock()
	if got := srv.governorNextPassIn(now); got != "13m" {
		t.Fatalf("eta = %q, want 13m", got)
	}
	if got := acceptTriageNextText("13m"); !strings.HasPrefix(got, "Hive picks it up on its next scanner pass (≈13m);") {
		t.Fatalf("next = %q", got)
	}
	if got := acceptTriageNextText(""); !strings.HasPrefix(got, "Hive picks it up on its next scanner pass;") {
		t.Fatalf("next without eta = %q", got)
	}
}
