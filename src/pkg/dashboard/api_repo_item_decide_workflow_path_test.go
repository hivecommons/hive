package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// hivecommons/hive#15203: a contributor head that edits `.github/workflows/`
// cannot be adopted (the App has no `workflows` permission), so "Let hive
// decide" must answer capability-gap, never queue an adopt-supersede.
func TestDelegatedDecisionWorkflowPathHeadAnswersCapabilityGap(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prFiles: []string{"README.md", ".github/workflows/ci.yml"}}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:decision-requested","caption":"Needs human decision"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Rule != "capability-gap" || job.Decision.Verdict != delegatedecision.VerdictRecommendOnly || !job.Decision.KeepsPark {
		t.Fatalf("decision = %+v status=%s err=%s", job.Decision, job.Status, job.Error)
	}
	if job.Result == nil || job.Result.Action != rowActionRecommended {
		t.Fatalf("result = %+v", job.Result)
	}
	if strings.Contains(strings.Join(mock.added, ","), delegatedecision.LabelAdoptSupersede) {
		t.Fatalf("adoption queued for a head the App cannot push: %v", mock.added)
	}
}

// The same offline contributor with an ordinary diff still adopts: the file
// list, not the park alone, decides capability-gap.
func TestDelegatedDecisionNonWorkflowHeadStillAdopts(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prRedCI: true, prMergeState: "blocked", prFiles: []string{"src/main.go"}}
	srv := newDelegatedServer(t, mock)
	w := postDelegated(t, srv, 8, "writer", `{"type":"pr","reason":"contributor-lane:decision-requested","caption":"Needs human decision"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	job := decodeDelegatedJob(t, w)
	if job.Decision == nil || job.Decision.Verdict != delegatedecision.VerdictAdoptSupersede {
		t.Fatalf("decision = %+v status=%s err=%s", job.Decision, job.Status, job.Error)
	}
}
