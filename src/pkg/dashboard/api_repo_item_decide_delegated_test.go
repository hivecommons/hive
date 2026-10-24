package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
)

func newDelegatedServer(t *testing.T, mock *repoItemDecisionMock) *Server {
	t.Helper()
	prev := delegatedDecisionRunAsync
	delegatedDecisionRunAsync = false
	t.Cleanup(func() { delegatedDecisionRunAsync = prev })
	return newRepoItemDecisionServer(t, mock)
}

func postDelegated(t *testing.T, srv *Server, number int, user, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/"+strconv.Itoa(number)+"/decide-delegated", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func decodeDelegatedJob(t *testing.T, w *httptest.ResponseRecorder) delegatedDecisionJob {
	t.Helper()
	var job delegatedDecisionJob
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v (%s)", err, w.Body.String())
	}
	return job
}

func TestDelegatedDecisionIssueDefaultApproveUnparksWithMarker(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictApprove || job.Decision.Rule != "default-approve" {
		t.Fatalf("job = %+v", job)
	}
	if job.Marker != "<!-- hive:delegated-decision by=h1 policy=owner-delegate -->" {
		t.Fatalf("marker = %q", job.Marker)
	}
	// The `/hive decision` comment with the marker lands first, then the
	// un-park reply the sweep would post; approved-direction and
	// triage/accepted are written; nothing closes.
	if len(mock.comments) != 2 || !strings.HasPrefix(mock.comments[0], "/hive decision ") || !strings.Contains(mock.comments[0], job.Marker) {
		t.Fatalf("comments = %v", mock.comments)
	}
	if !strings.Contains(mock.comments[1], "Un-parked by @writer") {
		t.Fatalf("unpark reply missing: %v", mock.comments)
	}
	joined := strings.Join(mock.added, ",")
	if !strings.Contains(joined, github.HumanAckLabel) || !strings.Contains(joined, delegatedecision.LabelTriageAccepted) {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.edits) != 0 {
		t.Fatalf("must not close: %v", mock.edits)
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionDelegated) {
		t.Fatalf("audit missing: %v", decisionAuditActions(srv))
	}

	// GET reads the same job back.
	req := httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/7/decide-delegated", nil)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || decodeDelegatedJob(t, rr).ID != job.ID {
		t.Fatalf("GET code=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/99/decide-delegated", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET unknown code=%d", rr.Code)
	}
}

func TestDelegatedDecisionHoldRailRefusesWithoutWrites(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"hold", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Decision.Verdict != delegatedecision.VerdictRefused || !job.Decision.KeepsPark {
		t.Fatalf("job = %+v", job)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 || len(mock.removed) != 0 || len(mock.edits) != 0 {
		t.Fatalf("held item must see no writes: %+v", mock)
	}
}

func TestDelegatedDecisionOwnerOnlyLabelsAndKeepsPark(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"GITHUB_TOKEN lacks packages:write for the release job"}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Decision.Verdict != delegatedecision.VerdictOwnerOnly || !job.Decision.KeepsPark {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
	joined := strings.Join(mock.added, ",")
	if !strings.Contains(joined, delegatedecision.LabelNeedsOwner) || !strings.Contains(joined, delegatedecision.LabelHold) || strings.Contains(joined, github.HumanAckLabel) {
		t.Fatalf("added = %v", mock.added)
	}
	if len(mock.comments) != 1 || strings.Contains(mock.comments[0], "/hive decision") || !strings.Contains(mock.comments[0], "owner action required") || !strings.Contains(mock.comments[0], "stays") {
		t.Fatalf("comments = %v", mock.comments)
	}
}

func TestDelegatedDecisionDuplicateCloses(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"looks like a duplicate of #3"}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Decision.Verdict != delegatedecision.VerdictCloseDuplicate {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], "Duplicate of #3") || !strings.Contains(mock.comments[0], job.Marker) {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 1 || mock.edits[0]["state"] != "closed" || mock.edits[0]["state_reason"] != issueStateReasonNotPlanned {
		t.Fatalf("edits = %v", mock.edits)
	}
}

func TestDelegatedDecisionPRHiveAuthoredQueuesApproval(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newDelegatedServer(t, mock)

	// A writer without the merger role goes through /lgtm + /approve.
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Status != delegatedJobStatusDone || job.Decision.Verdict != delegatedecision.VerdictApprove {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
	all := strings.Join(mock.comments, "\n")
	if !strings.Contains(all, "/hive decision approve #8") || !strings.Contains(all, job.Marker) || !strings.Contains(all, "/lgtm") || !strings.Contains(all, "/approve") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if len(mock.edits) != 0 {
		t.Fatalf("PR approval must not close or merge: %v", mock.edits)
	}
}

func TestDelegatedDecisionAuthAndSetting(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)

	if w := postDelegated(t, srv, 7, "", `{}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no user: code=%d", w.Code)
	}
	if w := postDelegated(t, srv, 7, "reader", `{}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := postDelegated(t, srv, 7, "writer", `{bad`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: code=%d", w.Code)
	}
	off := false
	srv.deps.Config.Decisions.Delegate.Enabled = &off
	if w := postDelegated(t, srv, 7, "writer", `{}`, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "decisions.delegate.enabled") {
		t.Fatalf("disabled: code=%d body=%s", w.Code, w.Body.String())
	}
	on := true
	srv.deps.Config.Decisions.Delegate.Repos = map[string]config.DelegateDecisionRepoConfig{"testorg/testrepo": {Enabled: &on}}
	if w := postDelegated(t, srv, 7, "writer", `{}`, nil); w.Code != http.StatusOK {
		t.Fatalf("repo override: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments) == 0 {
		t.Fatal("expected the decision to run once enabled per-repo")
	}
}

func TestDelegatedDecisionAsyncAnswersAcceptedAndSingleFlight(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/045-needs-decision"}}
	srv := newRepoItemDecisionServer(t, mock)
	srv.delegatedJobs.start(delegatedJobKey("testorg/testrepo", 7), &delegatedDecisionJob{ID: "running", Status: delegatedJobStatusRunning}, time.Now())
	w := postDelegated(t, srv, 7, "writer", `{}`, nil)
	if w.Code != http.StatusConflict || decodeDelegatedJob(t, w).ID != "running" {
		t.Fatalf("single-flight: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDelegatedJobsExpireFinishedEntries(t *testing.T) {
	var jobs delegatedDecisionJobs
	now := time.Now()
	old := now.Add(-2 * delegatedDecisionJobTTL)
	jobs.start("a#1", &delegatedDecisionJob{ID: "old", Status: delegatedJobStatusDone, EndedAt: &old}, now)
	if _, ok := jobs.get("a#1"); !ok {
		t.Fatal("old job should be present before sweep")
	}
	jobs.start("b#2", &delegatedDecisionJob{ID: "new", Status: delegatedJobStatusRunning}, now)
	if _, ok := jobs.get("a#1"); ok {
		t.Fatal("expired job should be swept on the next start")
	}
	jobs.finish("b#2", func(j *delegatedDecisionJob) { j.Status = delegatedJobStatusFailed; j.Error = "x" })
	got, _ := jobs.get("b#2")
	if got.Status != delegatedJobStatusFailed || got.EndedAt == nil {
		t.Fatalf("finish = %+v", got)
	}
	jobs.finish("missing", func(*delegatedDecisionJob) { t.Fatal("must not run") })
}

func TestDelegatedLowRiskPath(t *testing.T) {
	cases := map[string]bool{
		".github/workflows/ci.yml": true, "docs/x.md": true, "changelog.d/a.md": true, "README.md": true,
		"pkg/x/y.go": false, "ci/pipeline.yaml": true, "deploy/values.yaml": false,
	}
	for p, want := range cases {
		if got := delegatedLowRiskPath(p); got != want {
			t.Errorf("%s = %v want %v", p, got, want)
		}
	}
}

func TestDelegatedSupersedeQueuesAdoptionForForeignPR(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newDelegatedServer(t, mock)
	in := delegatedecision.Input{Repo: "testorg/testrepo", Number: 8, Kind: delegatedecision.KindPR, AdoptionStalled: true}
	d := delegatedecision.Run(in)
	if d.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision: %+v", d)
	}
	if _, err := srv.executeDelegatedDecision(context.Background(), in, d, "writer", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request not queued: %v", mock.added)
	}
	if len(mock.edits) != 0 {
		t.Fatalf("original PR edited: %v", mock.edits)
	}
}

func TestDelegatedSupersedeRefusedWhenAdoptionDisabled(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newDelegatedServer(t, mock)
	off := false
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = &off
	in := delegatedecision.Input{Repo: "testorg/testrepo", Number: 8, Kind: delegatedecision.KindPR, AdoptionStalled: true}
	d := delegatedecision.Run(in)
	if d.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision: %+v", d)
	}
	_, err := srv.executeDelegatedDecision(context.Background(), in, d, "writer", false)
	if err == nil || !strings.Contains(err.Error(), "adoption") {
		t.Fatalf("expected refusal with adoption disabled, got %v", err)
	}
	if strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request label queued despite disabled adoption: %v", mock.added)
	}
}
