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
	issueReads     int
	failRead       int
	failRemove     bool
	refreshLabels  []string
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
			m.issueReads++
			if m.failRead == m.issueReads {
				http.Error(w, `{"message":"refresh unavailable"}`, http.StatusBadGateway)
				return
			}
			if m.issueMissing {
				http.NotFound(w, r)
				return
			}
			issueLabels := m.issueLabels
			if m.issueReads > 1 && m.refreshLabels != nil {
				issueLabels = m.refreshLabels
			}
			labels := make([]map[string]string, 0, len(issueLabels))
			for _, label := range issueLabels {
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
			for _, label := range labels {
				if !acceptTriageHasLabel(m.issueLabels, label) {
					m.issueLabels = append(m.issueLabels, label)
				}
			}
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var label map[string]any
			_ = json.NewDecoder(r.Body).Decode(&label)
			if name, _ := label["name"].(string); name != "" {
				m.created = append(m.created, name)
			}
			_ = json.NewEncoder(w).Encode(label)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			if m.failRemove {
				http.Error(w, `{"message":"remove unavailable"}`, http.StatusBadGateway)
				return
			}
			part := strings.TrimPrefix(r.URL.EscapedPath(), "/repos/testorg/testrepo/issues/7/labels/")
			first, _ := url.PathUnescape(part)
			second, _ := url.PathUnescape(first)
			m.removed = append(m.removed, second)
			var remaining []string
			for _, label := range m.issueLabels {
				if label != second {
					remaining = append(remaining, label)
				}
			}
			m.issueLabels = remaining
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected GitHub API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghSrv.Close)
	srv := newFullServer(t)
	srv.deps.Config.HiveID = "h1"
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"testrepo"}
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
	if got := strings.Join(mock.added, ","); got != "triage/accepted,stage/010-actionable" {
		t.Fatalf("added = %q, want triage/accepted and the 010 stage label", got)
	}
	if got := strings.Join(mock.created, ","); got != "triage/accepted,stage/010-actionable" {
		t.Fatalf("created = %q, want the accept and stage labels ensured", got)
	}
	if got := strings.Join(mock.removed, ","); got != "needs-triage,stage/015-triage-needed" {
		t.Fatalf("removed = %q, want needs-triage and the 015 stage label", got)
	}
	if strings.Join(resp.Removed, ",") != "needs-triage,stage/015-triage-needed" || strings.Join(resp.Added, ",") != "triage/accepted,stage/010-actionable" {
		t.Fatalf("response labels = %+v", resp)
	}
	if got := labelStage(mock.issueLabels); got != stagelabels.Actionable {
		t.Fatalf("issue stage after accept = %v labels=%v, want 010 and exactly one stage label", got, mock.issueLabels)
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
	mock.issueLabels = []string{"needs-triage"}
	if w := postAcceptTriage(t, srv, "", true); w.Code != http.StatusOK {
		t.Fatalf("owner: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(mock.added, ",") != "triage/accepted,stage/010-actionable" {
		t.Fatalf("owner added = %v", mock.added)
	}
}

// An issue the reporter-trust gate parked at 000-new (no awaiting label, no
// stage label — hivecommons/hive#16652) leaves "New" in the same request:
// the handler writes stage/010-actionable with the accept label instead of
// waiting for the engine's next pass. A stage further along is never
// regressed.
func TestRepoItemAcceptTriageAdvancesStage(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", issueLabels: []string{"bug", "help wanted"}}
	srv := newAcceptTriageServer(t, mock)
	w := postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoAcceptTriageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if strings.Join(mock.added, ",") != "triage/accepted,stage/010-actionable" || len(mock.removed) != 0 {
		t.Fatalf("000-new issue: added=%v removed=%v", mock.added, mock.removed)
	}
	if resp.AlreadyAccepted || strings.Join(resp.Added, ",") != "triage/accepted,stage/010-actionable" {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionTriaged || !strings.Contains(resp.Result.Outcome, "`stage/010-actionable` added") {
		t.Fatalf("result = %+v", resp.Result)
	}

	// Accept label already on but no stage label (the state #16652 was left
	// in): a retry adds only the stage label and is not a no-op.
	mock.added, mock.removed, mock.created = nil, nil, nil
	mock.issueLabels = []string{"bug", "triage/accepted"}
	w = postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("retry: code=%d body=%s", w.Code, w.Body.String())
	}
	resp = repoAcceptTriageResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if strings.Join(mock.added, ",") != "stage/010-actionable" || len(mock.removed) != 0 || resp.AlreadyAccepted {
		t.Fatalf("retry: added=%v removed=%v resp=%+v", mock.added, mock.removed, resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionTriaged || !strings.HasPrefix(resp.Result.Outcome, "Triage already accepted: `stage/010-actionable` added") {
		t.Fatalf("retry result = %+v", resp.Result)
	}

	// Accepted and already 010: a true no-op.
	mock.added, mock.removed = nil, nil
	mock.issueLabels = []string{"bug", "triage/accepted", "stage/010-actionable"}
	w = postAcceptTriage(t, srv, "writer", false)
	resp = repoAcceptTriageResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || !resp.AlreadyAccepted || len(mock.added)+len(mock.removed) != 0 {
		t.Fatalf("already 010: code=%d resp=%+v added=%v removed=%v", w.Code, resp, mock.added, mock.removed)
	}

	// Claimed (020) is further along than 010: only the triage labels move.
	mock.added, mock.removed = nil, nil
	mock.issueLabels = []string{"needs-triage", "stage/020-claimed"}
	w = postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("claimed: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(mock.added, ",") != "triage/accepted" || strings.Join(mock.removed, ",") != "needs-triage" {
		t.Fatalf("claimed: added=%v removed=%v, must not regress 020", mock.added, mock.removed)
	}
	if got := labelStage(mock.issueLabels); got != stagelabels.Claimed {
		t.Fatalf("claimed stage after accept = %v labels=%v", got, mock.issueLabels)
	}
}

func TestRepoItemAcceptTriageIdempotentAndIssuesOnly(t *testing.T) {
	mock := &acceptTriageMock{permission: "maintain", issueLabels: []string{"bug", "triage/accepted", "stage/010-actionable"}}
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

	// Accepted label on, but the awaiting label lingers: the removal happens
	// and the stage-less issue is advanced to 010.
	mock.issueLabels = []string{"triage/accepted", "needs-triage"}
	w = postAcceptTriage(t, srv, "writer", false)
	if w.Code != http.StatusOK {
		t.Fatalf("lingering awaiting: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Join(mock.added, ",") != "stage/010-actionable" || strings.Join(mock.removed, ",") != "needs-triage" {
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
	if strings.Join(mock.added, ",") != "triage/ok,stage/010-actionable" {
		t.Fatalf("added = %v, want the first configured accept label and the 010 stage", mock.added)
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

	// The stage advance the handler writes: 010 for a stage-less or 015
	// issue, nothing for one further along.
	if got := acceptTriageStageAdvance([]string{"bug"}, "h1", labelStage); got != "stage/010-actionable" {
		t.Fatalf("stage-less advance = %q", got)
	}
	if got := acceptTriageStageAdvance([]string{"stage/015-triage-needed"}, "h1", labelStage); got != "stage/010-actionable" {
		t.Fatalf("015 advance = %q", got)
	}
	for _, l := range []string{"stage/010-actionable", "stage/020-claimed", "stage/040-covered-by-pr", "hold", "hive-pause/h1"} {
		if got := acceptTriageStageAdvance([]string{l}, "h1", labelStage); got != "" {
			t.Fatalf("%s advance = %q, want none", l, got)
		}
	}

	// Tracker derivation: triage/accepted with no stage label and no engine
	// record is 010, not 000-new (hivecommons/hive#16652); an awaiting label
	// still on, or a record / label stage, is left alone.
	none := stagelabels.Record{}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.None, none, false, []string{"bug", "triage/accepted"}); got != stagelabels.Actionable {
		t.Fatalf("accepted without stage = %v, want 010", got)
	}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.None, none, true, []string{"triage/accepted"}); got != stagelabels.Actionable {
		t.Fatalf("accepted with a None record = %v, want 010", got)
	}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.None, none, false, []string{"triage/accepted", "needs-triage"}); got != stagelabels.None {
		t.Fatalf("awaiting label still on = %v, want none", got)
	}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.None, none, false, []string{"bug"}); got != stagelabels.None {
		t.Fatalf("not accepted = %v, want none", got)
	}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.Claimed, none, false, []string{"triage/accepted"}); got != stagelabels.Claimed {
		t.Fatalf("label stage kept = %v", got)
	}
	if got := srv.stageTrackerAcceptedTriageStage(stagelabels.None, stagelabels.Record{Stage: stagelabels.IssueNeedsDecision}, true, []string{"triage/accepted"}); got != stagelabels.None {
		t.Fatalf("record stage wins = %v", got)
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

func TestAcceptTriageRefreshesWorkAndRetryAfterReload(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", issueLabels: []string{"needs-triage"}}
	var cached github.ActionableResult
	for attempt := 0; attempt < 2; attempt++ {
		// The second server models a restart with persisted GitHub labels but
		// an old dashboard snapshot. The ledger is not used for admission.
		srv := newAcceptTriageServer(t, mock)
		srv.deps.Config.Project.Org = "testorg"
		srv.deps.Config.Project.Repos = []string{"testrepo"}
		stale := &StatusPayload{Repos: []FrontendRepo{{
			Name: "testrepo", Full: "testorg/testrepo",
			NonActionableIssues: []any{github.RepoWorkIssue{Number: 7, Labels: []string{"needs-triage"}, Bucket: "reporter_triage"}},
		}}}
		srv.status = stale
		store := stagelabels.NewStore("", time.Now)
		store.Observe(stagelabels.Key("testorg/testrepo", 7), stagelabels.TriageNeeded, "triage needed")
		srv.stageStore = store
		srv.deps.ApplyWebhookItems = func(snaps []github.WebhookItemSnapshot) {
			for _, snap := range snaps {
				cached.ApplyWebhookItem(snap)
			}
		}
		gen := srv.stageTrackerGen
		epoch := srv.BeginStatusSnapshot()
		w := postAcceptTriage(t, srv, "writer", false)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: %d %s", attempt, w.Code, w.Body.String())
		}
		var resp repoAcceptTriageResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.AlreadyAccepted != (attempt == 1) || resp.MinStatusSeq == 0 || srv.BeginStatusSnapshot() <= epoch {
			t.Fatalf("attempt %d: response=%+v epoch=%d", attempt, resp, srv.BeginStatusSnapshot())
		}
		if srv.stageTrackerGen <= gen || !srv.stageTrackerCache.stale {
			t.Fatal("acceptance must invalidate the memoized tracker, including the no-op path")
		}
		it := trackerItem(t, srv.buildStageTracker(srv.status, store, time.Now()), "issue", 7)
		if it.Stage != stagelabels.Actionable.Number() || hasMod(it, modNeedsTriage) {
			t.Fatalf("accepted row still asks for triage: %+v", it)
		}
		if got := stale.Repos[0].NonActionableIssues[0].(github.RepoWorkIssue).Labels; strings.Join(got, ",") != "needs-triage" {
			t.Fatalf("refresh mutated a previously published status: %v", got)
		}
		if len(cached.Issues.Items) != 1 || cached.Issues.Items[0].Repo != "testrepo" || !acceptTriageHasLabel(cached.Issues.Items[0].Labels, "triage/accepted") {
			t.Fatalf("acceptance never reached classified work: %+v", cached.Issues)
		}
		olderEnumeration := &github.ActionableResult{
			GeneratedAt: time.Now().Add(-time.Hour),
			WorkDetailsByRepo: map[string]github.RepoWorkDetails{
				"testrepo": {Issues: []github.RepoWorkIssue{{Repo: "testrepo", Number: 7, Labels: []string{"needs-triage"}, Bucket: "reporter_triage"}}},
			},
		}
		olderEnumeration.ReplayWebhookPatches(&cached)
		if len(olderEnumeration.Issues.Items) != 1 {
			t.Fatal("an in-flight enumeration lost the acceptance patch")
		}
		// Exercise the persisted actionable snapshot representation, not just
		// a browser's optimistic row.
		data, err := json.Marshal(cached)
		if err != nil {
			t.Fatal(err)
		}
		cached = github.ActionableResult{}
		if err := json.Unmarshal(data, &cached); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(mock.added, ",") != "triage/accepted,stage/010-actionable" || strings.Join(mock.removed, ",") != "needs-triage" {
		t.Fatalf("retry repeated label writes: added=%v removed=%v", mock.added, mock.removed)
	}
}

func TestAcceptTriageHandoffFailuresAndConcurrentGates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mock          acceptTriageMock
		wantStatus    int
		wantBucket    github.WebhookItemBucket
		wantErrorText string
	}{
		{name: "read failure after write", mock: acceptTriageMock{issueLabels: []string{"needs-triage"}, failRead: 2}, wantStatus: http.StatusBadGateway, wantErrorText: "labels were saved"},
		{name: "read failure on retry", mock: acceptTriageMock{issueLabels: []string{"triage/accepted", "stage/010-actionable"}, failRead: 2}, wantStatus: http.StatusBadGateway, wantErrorText: "already accepted"},
		{name: "revoked during refresh", mock: acceptTriageMock{issueLabels: []string{"triage/accepted"}, refreshLabels: []string{"needs-triage"}}, wantStatus: http.StatusBadGateway, wantErrorText: "issue changed"},
		{name: "label removal failed", mock: acceptTriageMock{issueLabels: []string{"needs-triage"}, failRemove: true}, wantStatus: http.StatusBadGateway},
		{name: "hold remains", mock: acceptTriageMock{issueLabels: []string{"needs-triage", "hold"}}, wantStatus: http.StatusOK, wantBucket: github.WebhookBucketHeld},
		{name: "human gate remains", mock: acceptTriageMock{issueLabels: []string{"needs-triage", "needs-human"}}, wantStatus: http.StatusOK, wantBucket: github.WebhookBucketSidelined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.permission = "write"
			srv := newAcceptTriageServer(t, &tc.mock)
			srv.deps.GHClient.SetHoldLabels([]string{"hold"})
			var handed []github.WebhookItemSnapshot
			srv.deps.ApplyWebhookItems = func(snaps []github.WebhookItemSnapshot) { handed = append(handed, snaps...) }
			w := postAcceptTriage(t, srv, "writer", false)
			if w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), tc.wantErrorText) {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}

			if tc.wantStatus != http.StatusOK {
				if len(handed) != 0 || strings.Contains(w.Body.String(), `"ok":true`) {
					t.Fatal("failed handoff must not report success or publish unconfirmed work")
				}
				return
			}
			if len(handed) != 1 || handed[0].Patch.Bucket != tc.wantBucket {
				t.Fatalf("triage bypassed an independent gate: %+v", handed)
			}
			if strings.Join(tc.mock.removed, ",") != "needs-triage" {
				t.Fatalf("triage removed a hold/human gate: %v", tc.mock.removed)
			}
			// A held issue is "on hold" whatever its stage: no stage label
			// is written for the reconciler to strip again.
			wantAdded := "triage/accepted,stage/010-actionable"
			if tc.wantBucket == github.WebhookBucketHeld {
				wantAdded = "triage/accepted"
			}
			if got := strings.Join(tc.mock.added, ","); got != wantAdded {
				t.Fatalf("added = %q, want %q", got, wantAdded)
			}
		})
	}
}

func TestAcceptTriageRejectsUnconfiguredRepository(t *testing.T) {
	mock := &acceptTriageMock{issueLabels: []string{"triage/accepted"}}
	srv := newAcceptTriageServer(t, mock)
	srv.deps.Config.Project.Repos = []string{"another-repo"}
	var handed []github.WebhookItemSnapshot
	srv.deps.ApplyWebhookItems = func(snaps []github.WebhookItemSnapshot) { handed = append(handed, snaps...) }
	// Owning the target repo satisfies the existing permission gate, but is
	// not permission to insert work outside this hive's configured scope.
	w := postAcceptTriage(t, srv, "testorg", false)
	if w.Code != http.StatusForbidden || mock.issueReads != 0 || len(mock.added)+len(mock.removed)+len(handed) != 0 {
		t.Fatalf("unconfigured repo entered handoff: code=%d reads=%d handed=%v", w.Code, mock.issueReads, handed)
	}
}
