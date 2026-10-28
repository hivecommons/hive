package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14613 body, verbatim shape: no label, no marker, the
// keep-open sentence.
const trackingTestBody = "Tracking issue for the weekly competitor and SWOT report produced by the `weekly-competitor-report` agentic workflow.\n\n" +
	"Each run appends its report here as a comment so there is a searchable record in the repo.\n\n" +
	"Keep this issue open. If it is closed, the workflow creates a new tracking issue with the same title.\n"

func trackingTestIssue(number int, labels []string, body string) *gh.Issue {
	is := &gh.Issue{Number: gh.Ptr(number), State: gh.Ptr("open"), Title: gh.Ptr("Weekly competitor report"), Body: gh.Ptr(body), User: &gh.User{Login: gh.Ptr("owner")}}
	for _, l := range labels {
		is.Labels = append(is.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return is
}

func TestIsTrackingIssue(t *testing.T) {
	if why, ok := isTrackingIssue(nil); ok || why != "" {
		t.Fatalf("nil = %q,%v", why, ok)
	}
	if why, ok := isTrackingIssue(trackingTestIssue(1, nil, trackingTestBody)); !ok || why != "keep-open" {
		t.Fatalf("sentence = %q,%v", why, ok)
	}
	if why, ok := isTrackingIssue(trackingTestIssue(1, []string{gfi.TrackingLabel}, "")); !ok || why != "label" {
		t.Fatalf("label = %q,%v", why, ok)
	}
	if why, ok := isTrackingIssue(trackingTestIssue(1, nil, gfi.TrackingMarker)); !ok || why != "marker" {
		t.Fatalf("marker = %q,%v", why, ok)
	}
	// hivecommons/hive#14646: a GitHub Agentic Workflows issue, no hive label.
	ghaw := trackingTestIssue(14646, []string{"good first issue", "help wanted", "stage/010-actionable", gfi.GHAWLabel},
		"> This issue is automatically managed by GitHub Agentic Workflows. Do not close this issue manually.\n>\n> **No action to take** - Do not assign to an agent.\n\n<!-- gh-aw-detection-runs -->\n\n> - [x] expires <!-- gh-aw-expires: 2026-11-26T17:57:22.389Z --> on Nov 26, 2026, 5:57 PM UTC\n")
	ghaw.Title = gh.Ptr("[aw] Detection Runs")
	if why, ok := isTrackingIssue(ghaw); !ok || why != "gh-aw-marker" {
		t.Fatalf("gh-aw issue: %q,%v", why, ok)
	}
	ghaw.Body = gh.Ptr("This issue tracks detection runs.")
	if why, ok := isTrackingIssue(ghaw); !ok || why != "gh-aw-label" {
		t.Fatalf("gh-aw label + [aw] title: %q,%v", why, ok)
	}
	if !strings.Contains(trackingIssueLinkComment(14646, "gh-aw-marker"), "GitHub Agentic Workflows") {
		t.Fatal("nudge does not name GitHub Agentic Workflows")
	}
	if _, ok := isTrackingIssue(trackingTestIssue(1, []string{"bug"}, "crash on load")); ok {
		t.Fatal("ordinary bug classified as tracking")
	}
}

// Enumeration: a tracking issue lands in the filtered bucket with reason
// "tracking", is counted under Breakdown.Tracking, never becomes
// actionable, and derives stage None (outside the pipeline).
func TestEnumerateActionableFiltersTrackingIssue(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 14613, Title: "Weekly competitor report", User: wireUser{"owner"}, CreatedAt: hoursAgo(30), Body: trackingTestBody},
		{Number: 2, Title: "archive", User: wireUser{"owner"}, Labels: []wireLabel{{Name: gfi.TrackingLabel}}, CreatedAt: hoursAgo(3), Body: "label only"},
		{Number: 3, Title: "log", User: wireUser{"owner"}, CreatedAt: hoursAgo(3), Body: "marker only\n" + gfi.TrackingMarker},
		{Number: 4, Title: "real bug", User: wireUser{"owner"}, Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(3), Body: "the dashboard crashes on load"},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, []wirePR{}))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	for _, is := range result.Issues.Items {
		if is.Number != 4 {
			t.Fatalf("tracking issue #%d is actionable", is.Number)
		}
	}
	rows := map[int]RepoWorkIssue{}
	for _, row := range result.WorkDetailsByRepo[repo].Issues {
		rows[row.Number] = row
	}
	for _, n := range []int{14613, 2, 3} {
		r, ok := rows[n]
		if !ok || r.Bucket != workBucketFiltered || r.Reason != trackingFilterReason {
			t.Fatalf("#%d row = %+v, want filtered/%s", n, r, trackingFilterReason)
		}
		stage, covered, _ := IssueStageFor(IssueStageInput{Repo: org + "/" + repo, Number: n, Labels: r.Labels, Bucket: r.Bucket, Reason: r.Reason}, nil)
		if stage != stagelabels.None || covered {
			t.Fatalf("#%d stage = %v covered=%v, want None", n, stage, covered)
		}
	}
	if _, ok := rows[4]; ok {
		t.Fatalf("real bug was sidelined: %+v", rows[4])
	}
	b := result.WorkBreakdownByRepo[repo].Issues
	if b.Tracking != 3 || b.Filtered != 3 || b.Actionable != 1 {
		t.Fatalf("breakdown = %+v", b)
	}
}

func TestClassifyWebhookIssueSidelinesTrackingIssue(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()), "o", []string{"r"})
	issue := trackingTestIssue(14613, nil, trackingTestBody)
	p := c.classifyWebhookIssue("o/r", issue, issueLabelNames(issue.Labels), issue.GetCreatedAt().Time)
	if p.Bucket != WebhookBucketSidelined || p.WorkIssue == nil || p.WorkIssue.Bucket != workBucketFiltered || p.WorkIssue.Reason != trackingFilterReason {
		t.Fatalf("patch = %+v", p)
	}
}

// Close guard: every automated close path goes through CloseIssue, which
// refuses a tracking issue before any write; an explicit human decision
// may opt out.
func TestCloseIssueRefusesTrackingIssue(t *testing.T) {
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/14613" {
			_ = json.NewEncoder(w).Encode(trackingTestIssue(14613, nil, trackingTestBody))
			return
		}
		writes = append(writes, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 14613, "state": "closed"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "o/r", 14613, IssueCloseOptions{StateReason: IssueStateReasonCompleted})
	if !errors.Is(err, ErrTrackingIssue) {
		t.Fatalf("err = %v, want ErrTrackingIssue", err)
	}
	if len(writes) != 0 {
		t.Fatalf("writes before refusal: %v", writes)
	}

	err = c.CloseIssue(context.Background(), "o/r", 14613, IssueCloseOptions{StateReason: "not_planned", AllowTrackingIssue: true})
	if err != nil {
		t.Fatalf("AllowTrackingIssue: %v", err)
	}
	if len(writes) == 0 || !strings.HasPrefix(writes[0], "PATCH /repos/o/r/issues/14613") {
		t.Fatalf("writes = %v, want the close PATCH", writes)
	}
}

// Close-on-merge: the merged PR's claim on a tracking issue is a settled
// skip with reason tracking_issue, with no comment and no close.
func TestCloseOnMergeSkipsTrackingIssue(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/repos/o/r/issues/14613" {
			_ = json.NewEncoder(w).Encode(trackingTestIssue(14613, nil, trackingTestBody))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	pr := mergedTestPR(14615, "hive[bot]", "feat", "Adds the weekly report workflow.\n\nCloses #14613")
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}
	res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 14613, pr, true, opts)
	if res.Action != CloseOnMergeSkipped || res.Reason != "tracking_issue" {
		t.Fatalf("result = %+v, want tracking_issue skip", res)
	}
	if !res.Settles() {
		t.Fatalf("tracking skip must settle the claim so the sweep stops re-reading it: %+v", res)
	}
	if len(writes) != 0 {
		t.Fatalf("unexpected write requests: %v", writes)
	}
	if closeOnMergeBackfillCandidate(trackingTestIssue(14613, []string{LikelyDoneLabel}, trackingTestBody)) {
		t.Fatal("backfill must not pick up a tracking issue even when it wears likely-done")
	}
	if !closeOnMergeBackfillCandidate(trackingTestIssue(5, []string{LikelyDoneLabel}, "ordinary")) {
		t.Fatal("control: likely-done ordinary issue is a backfill candidate")
	}
}

// The hive-opened PR path rewrites Closes→Refs for a tracking issue.
func TestIncompleteIssueReasonTrackingIssue(t *testing.T) {
	got := incompleteIssueReason(trackingTestIssue(14613, nil, trackingTestBody))
	if !strings.Contains(got, "tracking/archive issue") || !strings.Contains(got, "keep-open") {
		t.Fatalf("reason = %q", got)
	}
	if got := incompleteIssueReason(trackingTestIssue(5, []string{"bug"}, "crash")); got != "" {
		t.Fatalf("control reason = %q", got)
	}
}

// The reviewer nudge: one marker comment per tracking issue the PR body
// closes, idempotent, never edits the PR body, ignores ordinary issues.
func TestNudgeTrackingIssueClosingRefs(t *testing.T) {
	var comments []string
	var edits int
	existing := "[]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/14613":
			_ = json.NewEncoder(w).Encode(trackingTestIssue(14613, nil, trackingTestBody))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5":
			_ = json.NewEncoder(w).Encode(trackingTestIssue(5, []string{"bug"}, "crash"))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/14743/comments":
			_, _ = w.Write([]byte(existing))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/14743/comments":
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			comments = append(comments, in.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": in.Body})
		case r.Method == http.MethodPatch:
			edits++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})

	body := "Rewrites the CI test module.\n\nCloses #14613\nFixes #5"
	found := c.NudgeTrackingIssueClosingRefs(context.Background(), "o/r", 14743, body, nil)
	if len(found) != 1 || found[0] != 14613 {
		t.Fatalf("found = %v", found)
	}
	if len(comments) != 1 || edits != 0 {
		t.Fatalf("comments = %d edits = %d", len(comments), edits)
	}
	if !strings.HasPrefix(comments[0], trackingIssueLinkMarker(14613)) || !strings.Contains(comments[0], "#14613 is a persistent tracking/archive issue") || !strings.Contains(comments[0], "Refs #14613") || !strings.Contains(comments[0], gfi.TrackingKeepOpenSentence) {
		t.Fatalf("comment = %q", comments[0])
	}

	// Second pass with the marker already on the PR: no second comment.
	existing = `[{"id":1,"body":` + strconvQuote(comments[0]) + `}]`
	found = c.NudgeTrackingIssueClosingRefs(context.Background(), "o/r", 14743, body, nil)
	if len(found) != 1 || len(comments) != 1 {
		t.Fatalf("second pass found = %v comments = %d", found, len(comments))
	}

	if got := c.NudgeTrackingIssueClosingRefs(context.Background(), "o/r", 14743, "Fixes #5", nil); got != nil {
		t.Fatalf("ordinary issue nudged: %v", got)
	}
	if got := (*Client)(nil).NudgeTrackingIssueClosingRefs(context.Background(), "o/r", 1, "Closes #14613", nil); got != nil {
		t.Fatalf("nil client: %v", got)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
