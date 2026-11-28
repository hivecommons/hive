package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// The direction "Let hive decide" records on a hive PR is the delegate's,
// not the person's who clicked: the record is marked Delegated, so a trusted
// approval of the same head afterwards supersedes it instead of the head
// staying at human-changes-requested with Approve disabled until a commit
// nobody was going to push.
func TestDelegatedDirectionIsRecordedAsDelegated(t *testing.T) {
	mock := &repoItemDecisionMock{prMergeState: "blocked", prRedCI: true}
	srv := newDelegatedServer(t, mock)

	w := postDelegated(t, srv, 8, "writer", `{"type":"pr"}`, nil)
	job := decodeDelegatedJob(t, w)
	if w.Code != http.StatusOK || job.Status != delegatedJobStatusDone || job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictDirection {
		t.Fatalf("code=%d job=%+v", w.Code, job)
	}
	req, ok := repoItemDecisionChangeRequests().For("testorg/testrepo", 8, "feedfacefeedfacefeedfacefeedfacefeedface")
	if !ok || req.IsApproval() || req.Actor != "writer" {
		t.Fatalf("direction record = %+v ok=%v", req, ok)
	}
	if !req.Delegated {
		t.Fatalf("the delegate's direction is recorded as the person's own: %+v", req)
	}
}
