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
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func newDelegatedServer(t *testing.T, mock *repoItemDecisionMock) *Server {
	t.Helper()
	prev := delegatedDecisionRunAsync
	delegatedDecisionRunAsync = false
	t.Cleanup(func() { delegatedDecisionRunAsync = prev })
	srv := newRepoItemDecisionServer(t, mock)
	// PR decisions fail closed without an enumeration snapshot.
	srv.status = &StatusPayload{}
	return srv
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
	// The shared row-action result says what hive decided, on what, and
	// what happens next (the dashboard's persistent toast).
	if job.Result == nil || job.Result.Action != rowActionApproved || job.Result.Rule != "default-approve" || job.Result.Target.Kind != rowActionTargetIssue || job.Result.Target.Number != 7 || job.Result.Outcome == "" || job.Result.Next == "" {
		t.Fatalf("result = %+v", job.Result)
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

// GitHub says "blocked" for a missing required review and for red required
// checks alike; the delegate must read the head's checks before calling it
// green, and never submit an approval on a red or unproven head.
func TestDelegatedDecisionPRBlockedStateReadsHeadChecks(t *testing.T) {
	cases := []struct {
		name        string
		mergeState  string
		redCI, noCI bool
		wantVerdict delegatedecision.Verdict
		wantRule    string
	}{
		{"blocked by red required checks", "blocked", true, false, delegatedecision.VerdictDirection, "fix-then-merge"},
		{"blocked with checks never run", "blocked", false, true, delegatedecision.VerdictDirection, "fix-then-merge"},
		{"unknown state with checks never run", "unknown", false, true, delegatedecision.VerdictDirection, "fix-then-merge"},
		{"unstable head", "unstable", false, false, delegatedecision.VerdictDirection, "fix-then-merge"},
		{"blocked by missing review only", "blocked", false, false, delegatedecision.VerdictApprove, "default-release"},
		{"clean head", "clean", false, false, delegatedecision.VerdictApprove, "default-release"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{prMergeState: tc.mergeState, prRedCI: tc.redCI, prNoCI: tc.noCI}
			srv := newDelegatedServer(t, mock)

			w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
			job := decodeDelegatedJob(t, w)
			if w.Code != http.StatusOK || job.Status != delegatedJobStatusDone {
				t.Fatalf("code=%d job=%+v", w.Code, job)
			}
			if job.Decision.Verdict != tc.wantVerdict || job.Decision.Rule != tc.wantRule {
				t.Fatalf("verdict=%s rule=%s, want %s/%s", job.Decision.Verdict, job.Decision.Rule, tc.wantVerdict, tc.wantRule)
			}
			all := strings.Join(mock.comments, "\n")
			approved := strings.Contains(all, "/approve")
			if tc.wantVerdict == delegatedecision.VerdictApprove && !approved {
				t.Fatalf("expected approval on a green head; comments = %v", mock.comments)
			}
			if tc.wantVerdict != delegatedecision.VerdictApprove {
				if approved || !strings.Contains(all, "make CI green") {
					t.Fatalf("a red or unproven head must get direction, not approval; comments = %v", mock.comments)
				}
			}
		})
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

// TestDelegatedDecisionRepeatIsNoop (owner request): clicking "Let hive
// decide" again after the same rule already decided the item records
// nothing new and says so plainly, with when the decision was first
// recorded and what still blocks the item.
func TestDelegatedDecisionRepeatIsNoop(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T12:10:00Z")
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	mock.priorComments = []string{delegatedecision.CommentBody(delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve", Rationale: "r"}, "h1", "writer")}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoop {
		t.Fatalf("result = %+v", job.Result)
	}
	if !strings.HasPrefix(job.Result.Outcome, "No change — hive already recorded this decision") || !strings.Contains(job.Result.Outcome, "2026-10-28T12:01:00Z") {
		t.Fatalf("outcome = %q", job.Result.Outcome)
	}
	// Inside the effect window the answer says what was decided, in plain
	// words, and by when it should show — no bare rule slug.
	if job.Result.AppliedAt == nil || !strings.Contains(job.Result.Next, "Expected by 2026-10-28T12:31:00Z") || strings.Contains(job.Result.Outcome, "`default-approve`") {
		t.Fatalf("next = %q outcome = %q applied_at = %v", job.Result.Next, job.Result.Outcome, job.Result.AppliedAt)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 {
		t.Fatalf("a repeat must write nothing: comments=%v added=%v", mock.comments, mock.added)
	}
}

// An issue row with two open nested PRs (hivecommons/hive#15009: #15017 and
// #15020 under one issue) must not have the delegate silently pick one:
// the job decides each PR on its own, lists every outcome under `items`,
// registers each PR's job so GET on the PR path reads it, and summarises
// them in `result` for the issue row's toast.
func TestDelegatedDecisionIssueWithTwoPRsDecidesEach(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug"}}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human","prs":[9,8,8,7]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Decision != nil || job.Error != "" {
		t.Fatalf("job = %+v", job)
	}
	if len(job.Items) != 2 || job.Items[0].Number != 8 || job.Items[1].Number != 9 {
		t.Fatalf("items = %+v (sorted, deduped, never the issue itself)", job.Items)
	}
	for _, it := range job.Items {
		if it.Type != "pr" || it.Status != delegatedJobStatusDone || it.Decision == nil || it.Decision.Verdict != delegatedecision.VerdictApprove ||
			it.Result == nil || it.Result.Target.Kind != rowActionTargetPR || it.Result.Target.Number != it.Number || it.Result.Action != rowActionApproved {
			t.Fatalf("item = %+v", it)
		}
	}
	if job.Result == nil || job.Result.Action != rowActionDecidedEach || job.Result.Target.Kind != rowActionTargetIssue || job.Result.Target.Number != 7 {
		t.Fatalf("result = %+v", job.Result)
	}
	if !strings.Contains(job.Result.Outcome, "#8 approved") || !strings.Contains(job.Result.Outcome, "#9 approved") || !strings.Contains(job.Result.Next, "2 sent · 2 decided · 0 no-op") {
		t.Fatalf("outcome = %q next = %q", job.Result.Outcome, job.Result.Next)
	}
	all := strings.Join(mock.comments, "\n")
	if !strings.Contains(all, "/hive decision approve #8") || !strings.Contains(all, "/hive decision approve #9") {
		t.Fatalf("each PR gets its own decision comment: %v", mock.comments)
	}
	if strings.Contains(all, "approve #7") {
		t.Fatalf("the issue itself must not be decided: %v", mock.comments)
	}
	// Each PR's job is readable on its own path.
	for _, n := range []int{8, 9} {
		rr := httptest.NewRecorder()
		srv.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/items/"+strconv.Itoa(n)+"/decide-delegated", nil))
		sub := decodeDelegatedJob(t, rr)
		if rr.Code != http.StatusOK || sub.Number != n || sub.Status != delegatedJobStatusDone || sub.Result == nil || sub.Result.Target.Number != n {
			t.Fatalf("GET #%d code=%d job=%+v", n, rr.Code, sub)
		}
	}
	if !strings.Contains(strings.Join(decisionAuditActions(srv), ","), auditRepoItemDecisionDelegated) {
		t.Fatalf("audit missing: %v", decisionAuditActions(srv))
	}
}

// A single nested PR, or none, keeps the issue decision as it was.
func TestDelegatedDecisionIssueWithOnePRHintDecidesIssue(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug", "stage/045-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","prs":[8]}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Status != delegatedJobStatusDone || len(job.Items) != 0 || job.Result == nil || job.Result.Target.Number != 7 || job.Result.Target.Kind != rowActionTargetIssue {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
}

// Without a hint the server reads the issue's linked PRs from the tracker
// pairing the row was drawn from.
func TestDelegatedNestedPRNumbersFromTracker(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "o/r", Kind: "issue", Number: 1, LinkedPRs: []StageTrackerLinkedPR{{Number: 17}, {Number: 20}}},
		{Repo: "o/r", Kind: "pr", Number: 17},
		{Repo: "o/r", Kind: "issue", Number: 2},
	}
	if got := delegatedNestedPRNumbers(items, "O/R", 1); len(got) != 2 || got[0] != 17 || got[1] != 20 {
		t.Fatalf("got %v", got)
	}
	if got := delegatedNestedPRNumbers(items, "o/r", 2); len(got) != 0 {
		t.Fatalf("issue without PRs: %v", got)
	}
	if got := delegatedNestedPRNumbers(items, "o/r", 17); got != nil {
		t.Fatalf("a PR is not an issue: %v", got)
	}
	srv := &Server{}
	if got := srv.delegatedNestedOpenPRs("o/r", 1, []int{20, 17, 1, 0, 17}); len(got) != 2 || got[0] != 17 || got[1] != 20 {
		t.Fatalf("hint: %v", got)
	}
	res := delegatedPerPRResult("o/r", 1, []delegatedDecisionItem{
		{Number: 17, Result: &RowActionResult{Action: rowActionApproved, Rule: "small-low-risk"}},
		{Number: 20, Result: &RowActionResult{Action: rowActionNoop}},
		{Number: 21, Error: "GitHub 502"},
	})
	if res.Action != rowActionDecidedEach || res.Outcome != "Decided each of the 3 nested pull requests: #17 approved (small low risk); #20 no change (already decided); #21 failed — GitHub 502." {
		t.Fatalf("outcome = %q", res.Outcome)
	}
	if !strings.HasPrefix(res.Next, "3 sent · 1 decided · 1 no-op · 1 failed") {
		t.Fatalf("next = %q", res.Next)
	}
}

func pinDelegatedClock(t *testing.T, at string) {
	t.Helper()
	now, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	prev := delegatedDecisionNow
	delegatedDecisionNow = func() time.Time { return now }
	t.Cleanup(func() { delegatedDecisionNow = prev })
}

// TestDelegatedDecisionStuckReportsNoEffect reproduces hivecommons/hive#14822:
// the hive decided `supersede` on a contributor PR at 12:01Z; the PR is still
// parked with `contributor-lane:changes-requested` (the merge gate read the
// hive's own decision as a reviewer's change request) and the author is
// online, so the supersede never ran. Past decisions.delegate.effect_timeout
// the repeat click answers `no-effect` with the plain why and the options,
// and the hive posts one plain-words follow-up on the PR — not a second
// decision.
func TestDelegatedDecisionStuckReportsNoEffect(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	prior := delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline", Text: "supersede now: adopt #8 in supersede mode", Rationale: "offline"}
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	mock.priorComments = []string{delegatedecision.CommentBody(prior, "h1", "writer")}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	body := `{"type":"pr","reason":"contributor-lane:changes-requested","caption":"changes requested"}`
	w := postDelegated(t, srv, 8, "writer", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoEffect {
		t.Fatalf("result = %+v notes=%v", job.Result, job.Notes)
	}
	res := job.Result
	t.Logf("outcome: %s\nnext: %s\nwhy: %s", res.Outcome, res.Next, res.Why)
	for _, want := range []string{"Close this PR and open a fresh hive-authored PR with the same commits at 2026-10-28T12:01:00Z", "but it hasn't taken effect:"} {
		if !strings.Contains(res.Outcome, want) {
			t.Fatalf("outcome %q missing %q", res.Outcome, want)
		}
	}
	if strings.Contains(res.Outcome, "contributor-offline") || strings.Contains(res.Outcome, "supersede") {
		t.Fatalf("bare slug or jargon in outcome: %q", res.Outcome)
	}
	if res.Why == "" || res.Why == delegatedecision.GlossaryEffectUnknown {
		t.Fatalf("why = %q", res.Why)
	}
	labels := optionLabels(res.Options)
	for _, want := range []string{"Merge now as maintainer", "Re-run decision", "Close as superseded", "Send back for a new commit"} {
		if !strings.Contains(strings.Join(labels, "|"), want) || !strings.Contains(res.Next, want) {
			t.Fatalf("options %v / next %q missing %q", labels, res.Next, want)
		}
	}
	var rerun *StageTrackerDecisionOption
	for i := range res.Options {
		if res.Options[i].Action == decisionActionDelegateRerun {
			rerun = &res.Options[i]
		}
	}
	if rerun == nil || !rerun.Enabled {
		t.Fatalf("re-run option = %+v", rerun)
	}
	// One follow-up comment, plain words, stalled marker, never a decision.
	if len(mock.comments) != 1 || len(mock.added) != 0 {
		t.Fatalf("comments=%v added=%v", mock.comments, mock.added)
	}
	c := mock.comments[0]
	t.Logf("follow-up:\n%s", c)
	if !delegatedecision.IsStalledComment(c, "contributor-offline") || delegatedecision.IsDelegatedComment(c) || strings.HasPrefix(c, "/hive decision") {
		t.Fatalf("follow-up = %q", c)
	}
	for _, want := range []string{"hasn't taken effect", "What should have happened", "Why it hasn't", "What you can do", "Re-run decision"} {
		if !strings.Contains(c, want) {
			t.Fatalf("follow-up missing %q:\n%s", want, c)
		}
	}

	// Second click: the follow-up is already there, nothing is posted again.
	mock.priorComments = append(mock.priorComments, c)
	mock.comments = nil
	w = postDelegated(t, srv, 8, "writer", body, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoEffect || len(mock.comments) != 0 {
		t.Fatalf("second click: result=%+v comments=%v", job.Result, mock.comments)
	}

	// force=true re-runs the decider past the dedup and writes a fresh
	// decision comment carrying the plain-words block.
	mock.comments = nil
	w = postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:changes-requested","force":true}`, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action == rowActionNoEffect || job.Result.Action == rowActionNoop {
		t.Fatalf("forced re-run did not decide: %+v notes=%v err=%s", job.Result, job.Notes, job.Error)
	}
}

// TestDelegatedDecisionReparkedAfterDecisionIsANewAsk is
// hivecommons/hive#15021: the hive decided at 12:01Z and the park lifted;
// later the scanner parked the item again with its own question. The
// repeat click must not read the fresh park as the old decision failing to
// take effect (no `no-effect`, no stalled follow-up comment): it is a new
// ask, decided anew.
func TestDelegatedDecisionReparkedAfterDecisionIsANewAsk(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	prior := delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede, Rule: "contributor-offline", Text: "supersede now: adopt #8 in supersede mode", Rationale: "offline"}
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	mock.priorComments = []string{delegatedecision.CommentBody(prior, "h1", "writer")}
	mock.timeline = []map[string]any{
		{"event": "unlabeled", "label": map[string]any{"name": "stage/085-needs-decision"}, "created_at": "2026-10-28T12:01:30Z"},
		{"event": "labeled", "label": map[string]any{"name": "stage/085-needs-decision"}, "created_at": "2026-10-28T13:16:00Z"},
	}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	body := `{"type":"pr","reason":"contributor-lane:changes-requested","caption":"changes requested"}`
	w := postDelegated(t, srv, 8, "writer", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action == rowActionNoEffect || job.Result.Action == rowActionNoop {
		t.Fatalf("a re-parked item must be decided anew, got %+v notes=%v err=%s", job.Result, job.Notes, job.Error)
	}
	if !strings.Contains(strings.Join(job.Notes, "\n"), "parked again at 2026-10-28T13:16:00Z (stage/085-needs-decision) after the decision recorded at 2026-10-28T12:01:00Z") {
		t.Fatalf("notes = %v, want the re-park named", job.Notes)
	}
	for _, c := range mock.comments {
		if delegatedecision.IsStalledComment(c, "contributor-offline") {
			t.Fatalf("stalled follow-up posted on a fresh ask:\n%s", c)
		}
	}

	// A park label applied before the decision is the one it answered:
	// the dedup still holds.
	mock.timeline = []map[string]any{
		{"event": "labeled", "label": map[string]any{"name": "stage/085-needs-decision"}, "created_at": "2026-10-28T11:00:00Z"},
	}
	mock.comments = nil
	w = postDelegated(t, srv, 8, "writer", body, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoEffect {
		t.Fatalf("park before the decision must still dedup: %+v", job.Result)
	}
}

func TestReparkFromEvents(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	events := []github.IssueEvent{
		{Event: "labeled", Label: "triage/accepted", At: t0.Add(time.Minute)},
		{Event: "unlabeled", Label: "stage/045-needs-decision", At: t0.Add(2 * time.Minute)},
		{Event: "labeled", Label: "needs-human", At: t0.Add(time.Hour)},
		{Event: "labeled", Label: "Stage/045-Needs-Decision", At: t0.Add(2 * time.Hour)},
	}
	got, ok := reparkFromEvents(events)
	if !ok || got.Label != "Stage/045-Needs-Decision" || !got.At.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("got %+v ok=%v, want the latest park label", got, ok)
	}
	if _, ok := reparkFromEvents(events[:2]); ok {
		t.Fatal("a triage label and an unlabel are not a re-park")
	}
}

// TestDelegatedDecisionStuckIssueWording covers an approved issue with both
// approved-direction and stage/045-needs-decision, with no later re-park
// recorded. Unlike hivecommons/hive#15021, this fixture has no timeline
// evidence that the park lifted and was added again. Past the effect timeout the
// toast must not say "approved for merge" (nothing merges on an issue),
// must name the stuck label, and must offer the lift first.
func TestDelegatedDecisionStuckIssueWording(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T22:00:00Z")
	prior := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve", Text: "approve", Rationale: "default"}
	mock := &repoItemDecisionMock{issueLabels: []string{"help wanted", "triage/accepted", "approved-direction", "stage/045-needs-decision"}}
	mock.priorComments = []string{delegatedecision.CommentBodyWith(prior, "h1", "writer", delegatedecision.CommentOptions{Kind: delegatedecision.KindIssue})}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"needs-decision","caption":"Needs decision"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoEffect {
		t.Fatalf("result = %+v notes=%v", job.Result, job.Notes)
	}
	res := job.Result
	t.Logf("outcome: %s\nnext: %s\nwhy: %s", res.Outcome, res.Next, res.Why)
	if res.Why != delegatedecision.GlossaryParkLabelStuck {
		t.Fatalf("why = %q", res.Why)
	}
	for _, bad := range []string{"approved for merge", "merge gate", "Hive decided"} {
		if strings.Contains(res.Outcome, bad) {
			t.Fatalf("PR wording / repeated headline in issue outcome %q: %q", res.Outcome, bad)
		}
	}
	for _, want := range []string{"Approved to proceed — an agent may claim it at 2026-10-28T", "stage/045-needs-decision is still on the issue"} {
		if !strings.Contains(res.Outcome, want) {
			t.Fatalf("outcome %q missing %q", res.Outcome, want)
		}
	}
	if !strings.Contains(res.Next, "approved-direction") || !strings.Contains(res.Next, "Lift the park") {
		t.Fatalf("next = %q", res.Next)
	}
	if len(res.Options) < 4 || res.Options[0].Action != decisionActionApprove || !res.Options[0].Enabled || res.Options[1].Action != decisionActionRetry {
		t.Fatalf("options = %+v", res.Options)
	}
	if labels := strings.Join(optionLabels(res.Options), "|"); !strings.Contains(labels, "Re-run decision") || !strings.Contains(labels, "Give direction") || strings.Contains(labels, "Merge as maintainer") {
		t.Fatalf("options = %q", labels)
	}
	// The follow-up comment carries the issue words too.
	if len(mock.comments) != 1 || strings.Contains(mock.comments[0], "approved for merge") || !strings.Contains(mock.comments[0], "approved to proceed") {
		t.Fatalf("comments = %v", mock.comments)
	}
}

// TestDelegatedStuckWhyReadsTheLane covers the why derivation per lane
// reason and verdict so every stuck decision gets a specific plain reason.
func TestDelegatedStuckWhyReadsTheLane(t *testing.T) {
	srv := newDelegatedServer(t, &repoItemDecisionMock{})
	pr := delegatedecision.Input{Kind: delegatedecision.KindPR, CIGreen: true}
	issue := delegatedecision.Input{Kind: delegatedecision.KindIssue}
	approve := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove}
	supersede := delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede}
	selfDecision := delegatedPriorState{Found: true, Text: "/hive decision supersede now: adopt"}
	selfApprove := delegatedPriorState{Found: true, Text: "/hive decision approve"}
	cases := []struct {
		name  string
		in    delegatedecision.Input
		d     delegatedecision.Decision
		req   delegatedDecisionRequest
		prior delegatedPriorState
		want  string
	}{
		{"self decision read as changes", pr, approve, delegatedDecisionRequest{Reason: "contributor-lane:changes-requested"}, selfDecision, delegatedecision.GlossarySelfDecision},
		{"real changes requested", pr, approve, delegatedDecisionRequest{Reason: "changes_requested"}, selfApprove, delegatedecision.GlossaryChangesRequest},
		{"author online blocks adopt", func() delegatedecision.Input { i := pr; i.ContributorOnline = true; return i }(), supersede, delegatedDecisionRequest{}, selfDecision, delegatedecision.GlossaryAuthorOnline},
		{"sentinel", func() delegatedecision.Input { i := pr; i.SentinelAlert = true; return i }(), approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossarySentinel},
		{"conflicting", func() delegatedecision.Input { i := pr; i.Conflicting = true; return i }(), approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryConflicting},
		{"ci red", func() delegatedecision.Input { i := pr; i.CIGreen = false; return i }(), approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryCIRed},
		{"dco", pr, approve, delegatedDecisionRequest{Caption: "DCO missing"}, selfApprove, delegatedecision.GlossaryDCO},
		{"hold", pr, approve, delegatedDecisionRequest{Reason: "hold"}, selfApprove, delegatedecision.GlossaryHold},
		{"pr fallback", pr, approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryNotMergedYet},
		{"issue fallback", issue, approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryStillParked},
		{"issue park label outlived the approval", delegatedecision.Input{Kind: delegatedecision.KindIssue, Labels: []string{"help wanted", "approved-direction", "stage/045-needs-decision"}}, approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryParkLabelStuck},
		{"issue park label without the ack is just parked", delegatedecision.Input{Kind: delegatedecision.KindIssue, Labels: []string{"stage/045-needs-decision"}}, approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryStillParked},
		{"pr with both labels is not the issue case", func() delegatedecision.Input {
			i := pr
			i.Labels = []string{"approved-direction", "stage/045-needs-decision"}
			return i
		}(), approve, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryNotMergedYet},
		{"close fallback", pr, delegatedecision.Decision{Verdict: delegatedecision.VerdictCloseSuperseded}, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryNotClosedYet},
		{"direction fallback", pr, delegatedecision.Decision{Verdict: delegatedecision.VerdictDirection}, delegatedDecisionRequest{}, selfApprove, delegatedecision.GlossaryNoNewCommit},
	}
	for _, tc := range cases {
		got := srv.delegatedStuckWhy(tc.in, tc.d, tc.req, tc.prior)
		if got != tc.want {
			t.Errorf("%s: why = %q, want %q", tc.name, got, tc.want)
		}
		if _, ok := delegatedecision.Explain(got); !ok {
			t.Errorf("%s: why %q has no glossary entry", tc.name, got)
		}
	}
}

// "Let hive decide" on a PR at 090 Ready for human merge answers at once
// (hivecommons/hive#15078): no policy run, no comment, a no-effect result
// that says in plain words why the hive will not merge it and carries the
// merge answers as buttons — enabled for a merger, greyed for a writer.
// The head conflicts and adoption is off on this hive, so the hive can
// neither approve nor adopt it (hivecommons/hive#15173).
func TestDelegatedDecisionReadyForHumanMergeAnswersWithMergeOptions(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "dirty", prLabels: []string{"stage/090-ready-for-human-merge", "role/newcomer"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(false)
	body := `{"type":"pr","reason":"contributor-lane:author-association","caption":"Ready for human merge · untrusted author"}`
	w := postDelegated(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Status != delegatedJobStatusDone || job.Result == nil || job.Result.Action != rowActionNoEffect {
		t.Fatalf("job = %+v", job)
	}
	res := job.Result
	t.Logf("outcome: %s\nnext: %s\nwhy: %s", res.Outcome, res.Next, res.Why)
	for _, want := range []string{"I can't merge this: author @someone is untrusted on testrepo", "isn't a trusted author yet", "trusted tier under 👥 Contributors"} {
		if !strings.Contains(res.Outcome, want) {
			t.Errorf("outcome %q missing %q", res.Outcome, want)
		}
	}
	if res.Why != delegatedecision.GlossaryHumanMerge || res.Rule != delegatedHumanMergeRule {
		t.Errorf("why=%q rule=%q", res.Why, res.Rule)
	}
	if !strings.Contains(res.Next, "Approve and let hive merge · Merge now as maintainer") || !strings.Contains(res.Next, "promote the author to trusted") {
		t.Errorf("next = %q", res.Next)
	}
	if len(res.Options) < 2 || res.Options[0].Action != decisionActionQueueMerge || !res.Options[0].Enabled || res.Options[1].Action != decisionActionMergeNow || !res.Options[1].Enabled {
		t.Fatalf("options = %+v", res.Options)
	}
	for _, o := range res.Options {
		if o.Action == decisionActionDirection && (o.Enabled || o.ReasonIfDisabled == "") {
			t.Errorf("direction on a human PR must be disabled with a reason: %+v", o)
		}
	}
	if len(mock.comments) != 0 || len(mock.edits) != 0 || len(mock.reviews) != 0 || len(mock.merges) != 0 {
		t.Fatalf("nothing may be written: comments=%v edits=%v reviews=%v merges=%v", mock.comments, mock.edits, mock.reviews, mock.merges)
	}

	// A writer without the merger role gets the same answer with the merge
	// buttons greyed and the reason in plain words.
	w = postDelegated(t, srv, 8, "writer", body, nil)
	job = decodeDelegatedJob(t, w)
	if job.Result == nil || len(job.Result.Options) < 2 {
		t.Fatalf("job = %+v", job)
	}
	if o := job.Result.Options[0]; o.Enabled || o.ReasonIfDisabled != decisionWhyMergerRole {
		t.Errorf("merge-now for a writer = %+v", o)
	}
}

// All nested PR jobs already running is a fast finish with no GitHub calls.
// The async parent still needs a settled ledger entry rather than an orphaned
// pending entry (hivecommons/hive#15064).
func TestDelegatedAsyncFanOutSettlesOperatorAction(t *testing.T) {
	srv := newDelegatedServer(t, &repoItemDecisionMock{})
	delegatedDecisionRunAsync = true
	srv.operatorActionStore = newOperatorActionStore("")
	for _, number := range []int{17, 20} {
		key := delegatedJobKey("testorg/testrepo", number)
		srv.delegatedJobs.start(key, &delegatedDecisionJob{Status: delegatedJobStatusRunning}, time.Now())
	}
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","prs":[17,20]}`, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		got := srv.operatorActions().List("testorg/testrepo", 7)
		if len(got) == 1 && got[0].Outcome == operatorActionOutcomeFailed {
			if !strings.Contains(got[0].Detail, "already running") {
				t.Fatalf("missing failure detail: %+v", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("async action never settled: %+v", got)
		case <-tick.C:
		}
	}
}

// hivecommons/hive#15188: a delegated adopt-supersede is not "done" when
// the request label lands. The ledger reads `executing` until the adoption
// engine records a copy, then "adopted as #N"; a workflow-path PR is a
// capability gap and routes to the workflows blocker instead of a bare
// Needs human decision.
func TestDelegatedAdoptSupersedeReportsExecutingUntilAdopted(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	now := time.Date(2026, 10, 28, 14, 23, 0, 0, time.UTC)
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	srv.operatorActionStore = newOperatorActionStore("")
	srv.stageStore = stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("testorg/testrepo", 8)
	srv.stageStore.Observe(key, stagelabels.NeedsDecision, "adoption-stalled:contributor-lane:ci-red")

	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"adoption-stalled:contributor-lane:ci-red","caption":"adoption stalled"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision = %+v err=%s", job.Decision, job.Error)
	}
	if !strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("request label not queued: %v", mock.added)
	}
	acts := srv.operatorActions().List("testorg/testrepo", 8)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeExecuting || !strings.Contains(acts[0].Detail, "executing") {
		t.Fatalf("ledger after decision = %+v", acts)
	}

	// Before the engine runs: the row says the hive is carrying it out.
	items := []StageTrackerItem{{Repo: "testorg/testrepo", Number: 8, Stage: 85, Head: "aaa"}}
	srv.applyOperatorActions(items, now.Add(time.Minute))
	last := items[0].LastOperatorAction
	if last == nil || last.State != operatorActionStateExecuting || last.Progress == "" || last.Human != "" {
		t.Fatalf("row before adoption = %+v", last)
	}

	// The adoption engine opened the copy: the same record now reads
	// "adopted as #91" and is addressed, whatever stage the row sits at.
	srv.stageStore.MarkAdopted(key, "aaa", 91, now.Add(2*time.Minute))
	srv.invalidateStageTrackerCache()
	items = []StageTrackerItem{{Repo: "testorg/testrepo", Number: 8, Stage: 85, Head: "aaa"}}
	srv.applyOperatorActions(items, now.Add(3*time.Minute))
	last = items[0].LastOperatorAction
	if last == nil || last.State != operatorActionStateAddressed || last.Outcome != operatorActionOutcomeApplied || !strings.Contains(last.Progress, "adopted as #91") {
		t.Fatalf("row after adoption = %+v", last)
	}

	// The engine refused the head instead (workflow path): blocked on the
	// workflows capability, with what a person can do.
	srv.stageStore.ClearAdoption(key)
	srv.stageStore.MarkAdoptionEscalated(key, "aaa", "adoption-stalled:contributor-lane:workflow-path")
	srv.invalidateStageTrackerCache()
	items = []StageTrackerItem{{Repo: "testorg/testrepo", Number: 8, Stage: 85, Head: "aaa"}}
	srv.applyOperatorActions(items, now.Add(3*time.Minute))
	last = items[0].LastOperatorAction
	if last == nil || last.State != operatorActionStateBlocked || last.Blocker != operatorActionBlockerWorkflows || last.Human == "" || !strings.Contains(last.Progress, "could not adopt") {
		t.Fatalf("row after refusal = %+v", last)
	}
}

// A PR that edits .github/workflows gets a capability-gap recommendation,
// not an adopt-supersede the engine would refuse; the ledger is `blocked`.
func TestDelegatedWorkflowPathIsCapabilityGap(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	srv.operatorActionStore = newOperatorActionStore("")
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"adoption-stalled:contributor-lane:workflow-path","caption":"adoption stalled: contributor lane: workflow path"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Rule != delegatedecision.RuleCapabilityGap || !job.Decision.KeepsPark {
		t.Fatalf("decision = %+v err=%s", job.Decision, job.Error)
	}
	if strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("adoption queued despite the capability gap: %v", mock.added)
	}
	acts := srv.operatorActions().List("testorg/testrepo", 8)
	if len(acts) != 1 || acts[0].Outcome != operatorActionOutcomeBlocked {
		t.Fatalf("ledger = %+v", acts)
	}
	if got := srv.delegatedStuckWhy(delegatedecision.Input{Kind: delegatedecision.KindPR, WorkflowPath: true}, delegatedecision.Decision{Verdict: delegatedecision.VerdictAdoptSupersede}, delegatedDecisionRequest{}, delegatedPriorState{Found: true}); got != delegatedecision.GlossaryWorkflowPath {
		t.Fatalf("stuck why = %q", got)
	}
}

// hivecommons/hive#15096 N13: a resolving delegated decision on a PR takes
// `needs-human` and the 085 stage label off in the same write, or the next
// label-derived pass re-parks the row the decision just answered.
func TestDelegatedDecisionLiftsStaleNeedsHuman(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"needs-human", stagelabels.NeedsDecision.Label(), "kind/bug"}}
	srv := newDelegatedServer(t, mock)
	in := delegatedecision.Input{Repo: "testorg/testrepo", Number: 8, Kind: delegatedecision.KindPR, AdoptionStalled: true, Labels: mock.prLabels}
	d := delegatedecision.Run(in)
	if d.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision: %+v", d)
	}
	notes, err := srv.executeDelegatedDecision(context.Background(), in, d, "writer", false)
	if err != nil {
		t.Fatal(err)
	}
	removed := strings.Join(mock.removed, ",")
	if !strings.Contains(removed, "needs-human") || !strings.Contains(removed, "085-needs-decision") {
		t.Fatalf("park labels not lifted: removed=%v", mock.removed)
	}
	if strings.Contains(removed, "kind/bug") {
		t.Fatalf("unrelated label removed: %v", mock.removed)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "park labels lifted: needs-human") {
		t.Fatalf("notes: %v", notes)
	}

	// Recommend-only keeps the item with the person: nothing comes off.
	mock.removed = nil
	rec := delegatedecision.Decision{Verdict: delegatedecision.VerdictRecommendOnly, Rule: "hive-policy", Text: "x", Actions: []delegatedecision.Action{{Kind: delegatedecision.ActionRecommendOnly}}}
	if _, err := srv.executeDelegatedDecision(context.Background(), in, rec, "writer", false); err != nil {
		t.Fatal(err)
	}
	if len(mock.removed) != 0 {
		t.Fatalf("recommend-only lifted labels: %v", mock.removed)
	}
}

// hivecommons/hive#15289: the #15219 scenario end to end. A conflicting,
// offline clanker PR whose issue a merged PR already closed is closed as
// superseded with a note naming that PR — not adopted — and the job
// exposes the ladder.
func TestDelegatedAlreadyFixedClosesInsteadOfAdopting(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "dirty", prBody: "Fixes #42", fixedIssueClosedBy: 43,
		prLabels: []string{"author/clanker", "adoption/supersede", "stage/085-needs-decision", "needs-rebase"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"adoption-stalled:contributor-offline","caption":"adoption stalled"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	d := job.Decision
	if d == nil || d.Verdict != delegatedecision.VerdictCloseSuperseded || d.Rule != delegatedecision.RuleAlreadyFixed {
		t.Fatalf("decision = %+v err=%s notes=%v", d, job.Error, job.Notes)
	}
	if d.Ladder == nil || d.Ladder.Decision != delegatedecision.RuleAlreadyFixed || len(d.Ladder.RuledOut) != 0 || !strings.Contains(d.Ladder.Reason, "#42 was already fixed by merged PR #43 (25de3ba)") {
		t.Fatalf("ladder = %+v", d.Ladder)
	}
	if strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("adoption queued for an already-fixed PR: %v", mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "#42 was already fixed by merged PR #43 (25de3ba)") || !strings.Contains(mock.comments[0], "Thank you") {
		t.Fatalf("close note = %v", mock.comments)
	}
	closed := false
	for _, e := range mock.edits {
		if e["state"] == "closed" {
			closed = true
		}
	}
	if !closed || !strings.Contains(strings.Join(mock.added, ","), github.SupersededLabel) {
		t.Fatalf("PR not closed as superseded: edits=%v added=%v", mock.edits, mock.added)
	}
	if job.Result == nil || job.Result.Action != rowActionClosed || job.Result.Outcome != "Closed as superseded: #42 already fixed by #43 (25de3ba)." {
		t.Fatalf("result = %+v", job.Result)
	}
}

// Without a merged closer the same PR falls to the last rung — and the
// decision names the author's clanker, not a hive.
func TestDelegatedOfflineClankerAdoptsWithClankerWording(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T14:23:00Z")
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "dirty", prBody: "Fixes #42",
		prLabels: []string{"author/clanker", "stage/085-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"adoption-stalled:contributor-offline","caption":"adoption stalled"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	d := job.Decision
	if d == nil || d.Verdict != delegatedecision.VerdictAdoptSupersede || d.Rule != delegatedecision.RuleContributorOffline {
		t.Fatalf("decision = %+v err=%s", d, job.Error)
	}
	if d.Ladder == nil || len(d.Ladder.RuledOut) != 4 || !strings.Contains(strings.Join(d.Ladder.RuledOut, "\n"), "return-to-contributor: the author's clanker hasn't been connected since the nudge") {
		t.Fatalf("ladder = %+v", d.Ladder)
	}
	if strings.Contains(d.Rationale, "author's hive") || job.Result == nil || !strings.HasSuffix(job.Result.Outcome, " Why: the author's clanker hasn't been connected since the nudge.") {
		t.Fatalf("wording: rationale=%q result=%+v", d.Rationale, job.Result)
	}
}

// Owner-style judgment of parked issues (hivecommons/hive#15021 and the
// proposal rules): the fixture bodies are the real issues, trimmed.
const delegatedSelfreportBody = "<!-- hive:selfreport fp=929efbb7d686368f1108570c hive=hosted-ibm-alchemy-logg-ptoo -->\n\n" +
	"A hive hit an unusual runtime error (class `agent_login_required`) and is reporting it.\n\n" +
	"| Field | Value |\n|---|---|\n| hive | hosted-ibm-alchemy-logg-ptoo |\n" +
	"| spoke | [hosted-ibm-alchemy-logg-ptoo](https://hosted-ibm-alchemy-logg-ptoo.example) |\n| owner(s) | @clubanderson |\n"

const delegatedEvidenceFreeProposalBody = "## Finding\n\nThis is now the third time a freshly-landed shell-checker change shipped with arms no self-test pins (#248, #257, #295).\n\n## Recommendation\n\nAdd a mutation gate for the shell checkers.\n"

// A selfreport whose remedy is the spoke operator's: the hive posts the
// direction to the spoke owner, adds needs-owner, and writes nothing that
// would make the issue actionable — no `/hive decision` line, no
// approved-direction, the 045 park kept.
func TestDelegatedSelfreportOperatorAction(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"triage/accepted", "stage/045-needs-decision"},
		issueTitle: "[selfreport] agent_login_required: login required detected", issueBody: delegatedSelfreportBody}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictOperatorAction || job.Decision.Rule != "selfreport-operator-action" {
		t.Fatalf("decision = %+v (error %q)", job.Decision, job.Error)
	}
	if len(mock.comments) != 1 || strings.Contains(mock.comments[0], "/hive decision") || !strings.Contains(mock.comments[0], "@clubanderson: re-authenticate the agent provider") || !strings.Contains(mock.comments[0], "`stage/045-needs-decision` stays") {
		t.Fatalf("comments = %q", mock.comments)
	}
	added := strings.Join(mock.added, ",")
	if !strings.Contains(added, delegatedecision.LabelNeedsOwner) || strings.Contains(added, github.HumanAckLabel) || strings.Contains(added, delegatedecision.LabelHold) {
		t.Fatalf("labels added = %v: needs-owner only, never approved-direction or hold", mock.added)
	}
	if len(mock.removed) != 0 || len(mock.edits) != 0 {
		t.Fatalf("park must stay and nothing edited: removed=%v edits=%v", mock.removed, mock.edits)
	}
	if job.Result == nil || job.Result.Action != rowActionRecommended || !strings.Contains(job.Result.Outcome, "Operator action") || !strings.Contains(job.Result.Next, "waiting on the spoke owner") {
		t.Fatalf("result = %+v", job.Result)
	}
}

// The #15021 loop: the same selfreport decided again, still parked, with a
// label event after the first decision (the park was never lifted, so it is
// not a new ask). The hive writes nothing and answers "already decided;
// waiting on <who>" — never "it hasn't taken effect".
func TestDelegatedSelfreportRedecideIsNoopNotLoop(t *testing.T) {
	pinDelegatedClock(t, "2026-10-28T20:00:00Z")
	mock := &repoItemDecisionMock{issueLabels: []string{"needs-owner", "stage/045-needs-decision"},
		issueTitle: "[selfreport] agent_login_required: login required detected", issueBody: delegatedSelfreportBody}
	in := delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 7, Title: mock.issueTitle, Body: mock.issueBody, Author: "hive[bot]"}
	prior := delegatedecision.Run(in)
	mock.priorComments = []string{delegatedecision.CommentBodyWith(prior, "h1", "writer", delegatedecision.CommentOptions{Kind: delegatedecision.KindIssue})}
	mock.timeline = []map[string]any{
		{"event": "labeled", "label": map[string]any{"name": "stage/045-needs-decision"}, "created_at": "2026-10-28T13:16:00Z"},
	}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Result == nil || job.Result.Action != rowActionNoop {
		t.Fatalf("result = %+v", job.Result)
	}
	if !strings.HasPrefix(job.Result.Outcome, "No change — hive already recorded this decision at 2026-10-28T12:01:00Z") || strings.Contains(job.Result.Outcome, "hasn't taken effect") {
		t.Fatalf("outcome = %q", job.Result.Outcome)
	}
	if !strings.Contains(job.Result.Next, "Waiting on the spoke owner: @clubanderson") {
		t.Fatalf("next = %q", job.Result.Next)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 || len(mock.removed) != 0 {
		t.Fatalf("a repeat must write nothing: comments=%v added=%v removed=%v", mock.comments, mock.added, mock.removed)
	}
}

// A hive-filed proposal without an incident or recurrence is declined for
// now: the reason is posted and the issue is closed as not planned; no
// approval, no approved-direction.
func TestDelegatedProposalNotNowClosesAsNotPlanned(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"testing", "stage/045-needs-decision"},
		issueTitle: "[quality] Gate proposal: shell-checker arms that survive mutation", issueBody: delegatedEvidenceFreeProposalBody}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","reason":"requires_human"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictNotNow || job.Decision.Rule != "proposal-not-now" {
		t.Fatalf("decision = %+v (error %q)", job.Decision, job.Error)
	}
	if len(mock.comments) != 1 || strings.Contains(mock.comments[0], "/hive decision") || !strings.Contains(mock.comments[0], "no incident or recurrence justifies the cost") {
		t.Fatalf("comments = %q", mock.comments)
	}
	closed := false
	for _, e := range mock.edits {
		if e["state"] == "closed" && e["state_reason"] == issueStateReasonNotPlanned {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("issue not closed as not planned: edits=%v", mock.edits)
	}
	if strings.Contains(strings.Join(mock.added, ","), github.HumanAckLabel) {
		t.Fatalf("a declined proposal must not get approved-direction: %v", mock.added)
	}
	if job.Result == nil || job.Result.Action != rowActionClosed || !strings.Contains(job.Result.Outcome, "Declined for now") {
		t.Fatalf("result = %+v", job.Result)
	}
}
