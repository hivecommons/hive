package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

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
