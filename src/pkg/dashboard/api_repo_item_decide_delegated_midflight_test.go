package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// seedTrackerPR plants a PR row in the last served tracker body, the way
// delegatedMidFlight reads the review track.
func seedTrackerPR(t *testing.T, srv *Server, number int, item map[string]any) {
	t.Helper()
	item["repo"], item["kind"], item["number"] = "testorg/testrepo", "pr", number
	srv.stageTrackerCacheMu.Lock()
	defer srv.stageTrackerCacheMu.Unlock()
	var cur struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(srv.stageTrackerCache.body, &cur)
	items := []map[string]any{item}
	for _, other := range cur.Items {
		if n, _ := other["number"].(float64); int(n) != number {
			items = append(items, other)
		}
	}
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	srv.stageTrackerCache.body = body
}

// seedSettledTrackerPRs serves tracker rows whose review is approved and CI
// passing, with no head recorded so they match any head.
func seedSettledTrackerPRs(srv *Server, numbers ...int) {
	items := make([]map[string]any, 0, len(numbers))
	for _, n := range numbers {
		items = append(items, map[string]any{"repo": "testorg/testrepo", "kind": "pr", "number": n, "stage": 85,
			"review_activity": map[string]any{"state": "approved"}, "ci_activity": map[string]any{"state": "passing"}})
	}
	body, _ := json.Marshal(map[string]any{"items": items})
	srv.stageTrackerCacheMu.Lock()
	srv.stageTrackerCache.body = body
	srv.stageTrackerCacheMu.Unlock()
}

const midFlightHead = "feedfacefeedfacefeedfacefeedfacefeedface"

func assertMidFlightNoEffect(t *testing.T, mock *repoItemDecisionMock, job delegatedDecisionJob, wantText string) {
	t.Helper()
	if job.Status != delegatedJobStatusDone || job.Decision == nil || job.Result == nil {
		t.Fatalf("job = %+v", job)
	}
	if job.Decision.Verdict != delegatedecision.VerdictRefused || job.Decision.Rule != delegatedecision.RuleMidFlight || !job.Decision.KeepsPark {
		t.Fatalf("decision = %+v", job.Decision)
	}
	if job.Result.Action != rowActionNoEffect || job.Result.Rule != delegatedecision.RuleMidFlight || !strings.Contains(job.Result.Outcome, wantText) {
		t.Fatalf("result = %+v, want no-effect mentioning %q", job.Result, wantText)
	}
	if len(mock.comments) != 0 || len(mock.reviews) != 0 || len(mock.edits) != 0 || len(mock.merges) != 0 {
		t.Fatalf("a mid-flight PR was written to: comments=%v reviews=%v edits=%v merges=%v", mock.comments, mock.reviews, mock.edits, mock.merges)
	}
}

// hivecommons/hive#18889: "Let hive decide" on a PR whose CI is still
// running used to run the policy, which has no CI guard (CIRed() is false
// while checks are pending) and recorded an approval of a head nobody had
// finished checking.
func TestDelegatedDecisionPendingCIIsNoEffect(t *testing.T) {
	for _, force := range []string{"", `,"force":true`} {
		mock := &repoItemDecisionMock{prMergeState: "unstable", prCIRunning: true}
		srv := newDelegatedServer(t, mock)
		w := postDelegated(t, srv, 8, "writer", `{"type":"pr"`+force+`}`, nil)
		assertMidFlightNoEffect(t, mock, decodeDelegatedJob(t, w), "CI running")
	}
}

// A review that is queued (or being worked) on the current head is not a
// head the delegate may approve either, whatever the checks say.
func TestDelegatedDecisionQueuedReviewIsNoEffect(t *testing.T) {
	for _, state := range []string{"unassigned", "assigned", "reviewing", "adjudicating"} {
		t.Run(state, func(t *testing.T) {
			mock := &repoItemDecisionMock{}
			srv := newDelegatedServer(t, mock)
			seedTrackerPR(t, srv, 8, map[string]any{"head": midFlightHead, "stage": 85,
				"review_activity": map[string]any{"head": midFlightHead, "state": state, "perspectives_done": 0, "perspectives_total": 5}})
			w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
			assertMidFlightNoEffect(t, mock, decodeDelegatedJob(t, w), "review")
		})
	}
}

// A park (and its review) recorded for an earlier head does not count: the
// push since restarted both tracks.
func TestDelegatedDecisionStaleParkAfterPushIsNoEffect(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newDelegatedServer(t, mock)
	seedTrackerPR(t, srv, 8, map[string]any{"head": "0ldhead0ldhead0ldhead0ldhead0ldhead0ldh", "stage": 85, "modifiers": []string{"needs-decision"},
		"review_activity": map[string]any{"head": "0ldhead0ldhead0ldhead0ldhead0ldhead0ldh", "state": "human_decision"}})
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	assertMidFlightNoEffect(t, mock, decodeDelegatedJob(t, w), "not yet run")
}

// Settled CI and review on the current head: the delegate decides as before.
func TestDelegatedDecisionSettledHeadStillDecides(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newDelegatedServer(t, mock)
	seedTrackerPR(t, srv, 8, map[string]any{"head": midFlightHead, "stage": 85,
		"review_activity": map[string]any{"head": midFlightHead, "state": "approved"},
		"ci_activity":     map[string]any{"head": midFlightHead, "state": "passing"}})
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictApprove {
		t.Fatalf("job = %+v", job)
	}
}

// "Let hive decide — all N PRs" with PRs in flight: each one in flight is
// skipped with its own no-effect result, and the summary counts it.
func TestDelegatedDecisionAllPRsSkipsMidFlight(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"bug"}}
	srv := newDelegatedServer(t, mock)
	// #8 reviews on the current head; #9 is settled, but the tracker row is
	// keyed per PR, so seed #8 only and let #9 decide.
	seedTrackerPR(t, srv, 8, map[string]any{"head": midFlightHead, "stage": 85,
		"review_activity": map[string]any{"head": midFlightHead, "state": "unassigned"}})
	w := postDelegated(t, srv, 7, "writer", `{"type":"issue","prs":[8,9]}`, nil)
	job := decodeDelegatedJob(t, w)
	if len(job.Items) != 2 {
		t.Fatalf("items = %+v", job.Items)
	}
	skipped, decided := job.Items[0], job.Items[1]
	if skipped.Number != 8 || skipped.Result == nil || skipped.Result.Action != rowActionNoEffect || skipped.Result.Rule != delegatedecision.RuleMidFlight {
		t.Fatalf("#8 = %+v, want skipped as no-effect", skipped)
	}
	if decided.Number != 9 || decided.Decision == nil || decided.Decision.Verdict != delegatedecision.VerdictApprove {
		t.Fatalf("#9 = %+v, want decided", decided)
	}
	if job.Result == nil || !strings.Contains(job.Result.Outcome, "#8 skipped") || !strings.Contains(job.Result.Next, "1 skipped") {
		t.Fatalf("summary = %+v", job.Result)
	}
	all := strings.Join(mock.comments, "\n")
	if strings.Contains(all, "approve #8") || !strings.Contains(all, "approve #9") {
		t.Fatalf("only the settled PR is decided: %v", mock.comments)
	}
}

// A PR whose review state cannot be read at all (no tracker body served, the
// PR not in a built tracker) is not decided on: the guard fails closed.
func TestDelegatedDecisionUnreadableReviewStateFailsClosed(t *testing.T) {
	mock := &repoItemDecisionMock{}
	srv := newDelegatedServer(t, mock)
	srv.stageTrackerCacheMu.Lock()
	srv.stageTrackerCache.body = nil
	srv.stageTrackerCacheMu.Unlock()
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	assertMidFlightNoEffect(t, mock, decodeDelegatedJob(t, w), "review state not yet loaded")
}
