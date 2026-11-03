package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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

// TestRepoItemDecisionQueueMergeOnHivePRRecordsApprovalWithoutReview
// (hivecommons/hive#15666): "Approve and let hive merge" on the hive's own
// PR cannot post an App review (GitHub 422 "Can not approve your own pull
// request"), so no review is attempted: the queue note is left as a
// comment, the auto-merge label applied, and the operator's approval is
// recorded in the hive's store exactly as the plain Approve records it —
// the self-authored lane merges on that record once CI is green.
func TestRepoItemDecisionQueueMergeOnHivePRRecordsApprovalWithoutReview(t *testing.T) {
	mock := &repoItemDecisionMock{prLabels: []string{"stage/090-ready-for-human-merge"}}
	srv := newRepoItemDecisionServer(t, mock)

	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"queue-merge","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("queue-merge on the hive's PR: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "queued-for-merge" || resp.Label != srv.deps.GHClient.AutoMergeLabel() {
		t.Fatalf("response = %+v", resp)
	}
	if len(mock.reviews) != 0 {
		t.Fatalf("the App must not review its own pull request: %v", mock.reviews)
	}
	if len(mock.merges) != 0 {
		t.Fatalf("queue-merge must not merge: %v", mock.merges)
	}
	if !slices.Contains(resp.Released, "approval-recorded") || !slices.Contains(resp.Released, "admitted") {
		t.Fatalf("released = %v, want approval-recorded and admitted", resp.Released)
	}
	var queueNote, lgtm, approve int
	for _, c := range mock.comments {
		switch {
		case strings.HasPrefix(c, "Approved by @writer for Hive auto-merge on green CI."):
			queueNote++
		case strings.HasPrefix(c, "/lgtm\n"):
			lgtm++
		case strings.HasPrefix(c, "/approve\n"):
			approve++
		}
	}
	if queueNote != 1 || lgtm != 1 || approve != 1 {
		t.Fatalf("comments = %q, want the queue note plus /lgtm and /approve", mock.comments)
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
