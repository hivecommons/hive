package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/reviewgate"
)

// openChangeRequestReviews is PR #8's review list with a standing change
// request from "someone", whom the decision mock reports with write access.
const openChangeRequestReviews = `[{"id":42,"user":{"login":"someone"},"state":"CHANGES_REQUESTED","commit_id":"feedfacefeedfacefeedfacefeedfacefeedface","submitted_at":"2026-11-26T10:00:00Z"}]`

// TestRepoItemDecisionStatesOpenChangeRequestFirst pins hivecommons/hive#20067
// on the dashboard: Approve, Merge now and Queue merge on a PR with a
// writer's open change request answer 409 naming it, and nothing is
// approved, labelled, commented or merged.
func TestRepoItemDecisionStatesOpenChangeRequestFirst(t *testing.T) {
	for _, decision := range []string{"approve", "merge-now", "queue-merge"} {
		t.Run(decision, func(t *testing.T) {
			mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}, prReviews: openChangeRequestReviews}
			srv := newRepoItemDecisionServer(t, mock)
			w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"`+decision+`","type":"pr"}`)
			if w.Code != http.StatusConflict {
				t.Fatalf("code = %d body = %s, want 409", w.Code, w.Body.String())
			}
			var resp repoItemChangesRequestedResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.NeedsOverrideReason || resp.Status != repoItemStatusChangesRequested || len(resp.ChangeRequests) != 1 ||
				resp.ChangeRequests[0].Reviewer != "someone" || resp.ChangeRequests[0].ReviewID != 42 || !strings.Contains(resp.Error, "someone (review 42)") {
				t.Fatalf("response = %+v", resp)
			}
			if len(mock.merges)+len(mock.reviews)+len(mock.comments)+len(mock.added) != 0 {
				t.Fatalf("writes reached GitHub: merges=%v reviews=%v comments=%q labels=%v", mock.merges, mock.reviews, mock.comments, mock.added)
			}
			if actions := decisionAuditActions(srv); !slices.Contains(actions, auditRepoItemDecisionChangesRequestedBlocked) {
				t.Fatalf("audit = %v, want %s", actions, auditRepoItemDecisionChangesRequestedBlocked)
			}
		})
	}
}

// TestRepoItemDecisionOverrideWithReason: the same Merge now resent with an
// override reason merges, and the override is audited with the reason.
func TestRepoItemDecisionOverrideWithReason(t *testing.T) {
	t.Cleanup(func() { reviewgate.ClearOverride("testorg/testrepo", 8) })
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}, prReviews: openChangeRequestReviews}
	srv := newRepoItemDecisionServer(t, mock)
	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr","override_reason":"reviewer agreed on the call; follow-up filed"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s, want 200", w.Code, w.Body.String())
	}
	if len(mock.merges) != 1 {
		t.Fatalf("merges = %v, want one", mock.merges)
	}
	found := false
	for _, e := range srv.GetAudit().Recent(20) {
		if e.Action == auditRepoItemDecisionChangesRequestedOverride && strings.Contains(e.Detail, "reviewer agreed on the call") && strings.Contains(e.Detail, "42") {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit = %v, want the override with its reason", decisionAuditActions(srv))
	}
}

// TestRepoItemDecisionNoChangeRequestUnchanged: without an open change
// request the decision runs exactly as before and nothing extra is audited.
func TestRepoItemDecisionNoChangeRequestUnchanged(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"},
		prReviews: `[{"id":42,"user":{"login":"reader"},"state":"CHANGES_REQUESTED","submitted_at":"2026-11-26T10:00:00Z"}]`}
	srv := newRepoItemDecisionServer(t, mock)
	w := postRepoItemDecisionAs(t, srv, 8, "writer", "merger", `{"decision":"merge-now","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s; a read-only reviewer's change request must not block", w.Code, w.Body.String())
	}
	if actions := decisionAuditActions(srv); slices.Contains(actions, auditRepoItemDecisionChangesRequestedBlocked) || slices.Contains(actions, auditRepoItemDecisionChangesRequestedOverride) {
		t.Fatalf("audit = %v, want no change-request entries", actions)
	}
}
