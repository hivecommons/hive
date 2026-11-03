package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const admissionTestHead = "feedfacefeedfacefeedfacefeedfacefeedface"

// seedAdmissionRecord gives PR #8 the stage record the engine would have
// written, so the dashboard's Approve has somewhere to note the admission.
func seedAdmissionRecord(t *testing.T, srv *Server, stage stagelabels.Stage) *stagelabels.Store {
	t.Helper()
	now := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	srv.stageStore = store
	store.Observe(stagelabels.Key("testorg/testrepo", 8), stage, "contributor-lane:untrusted-author")
	return store
}

// An owner's Approve — either variant — on an untrusted-author PR is a trust
// override for that head (hivecommons/hive#15136): the stage record carries
// admitted_by: operator-approval:<login>, the response and audit say so,
// and the contributor lane reads it (no adoption, merge as-is).
func TestRepoItemDecisionApproveAdmitsTheHead(t *testing.T) {
	for _, tc := range []struct {
		name, body, role, status, audit string
	}{
		{name: "approve", body: `{"decision":"approve","type":"pr"}`, role: "", status: "approval-commented", audit: auditRepoItemDecisionApprove},
		{name: "queue-merge", body: `{"decision":"queue-merge","type":"pr"}`, role: "merger", status: "queued-for-merge", audit: auditRepoItemDecisionQueue},
		{name: "merge-now", body: `{"decision":"merge-now","type":"pr"}`, role: "merger", status: "merged", audit: auditRepoItemDecisionMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}}
			srv := newRepoItemDecisionServer(t, mock)
			store := seedAdmissionRecord(t, srv, stagelabels.ReadyForHumanMerge)

			w := postRepoItemDecisionAs(t, srv, 8, "writer", tc.role, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("%s: code=%d body=%s", tc.name, w.Code, w.Body.String())
			}
			var resp repoItemDecisionResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.Status != tc.status || resp.AdmittedBy != "operator-approval:writer" {
				t.Fatalf("response = %+v", resp)
			}
			rec, ok := store.Get(stagelabels.Key("testorg/testrepo", 8))
			if !ok || rec.Admission(admissionTestHead) != "operator-approval:writer" || rec.AdmittedAt.IsZero() {
				t.Fatalf("record = %+v", rec)
			}
			if rec.Admission("0000000000000000000000000000000000000000") != "" {
				t.Fatalf("admission must be per head: %+v", rec)
			}
			if tc.name == "approve" && !strings.Contains(strings.Join(resp.Released, ","), "admitted") {
				t.Fatalf("approve must report the admission: %+v", resp.Released)
			}
			details := decisionAuditDetails(srv)
			if len(details) == 0 || !strings.Contains(details[0], "admitted_by=operator-approval:writer") {
				t.Fatalf("audit must carry admitted_by: %v", details)
			}
		})
	}
}

// Without a stage record there is nothing to note on: the Approve still
// lands, and the response says no admission was written.
func TestRepoItemDecisionApproveWithoutRecordAdmitsNothing(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true}
	srv := newRepoItemDecisionServer(t, mock)
	srv.stageStore = stagelabels.NewStore("", time.Now)
	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"approve","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.AdmittedBy != "" || strings.Contains(strings.Join(resp.Released, ","), "admitted") {
		t.Fatalf("admission without a record: %+v", resp)
	}
}

// Re-author is the explicit, separate choice when the head cannot merge
// as-is: it queues adoption with its own note and never merges.
func TestRepoItemDecisionReauthorQueuesAdoptionWithItsOwnNote(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge"}}
	srv := newRepoItemDecisionServer(t, mock)
	if w := postRepoItemDecision(t, srv, 7, "writer", `{"decision":"re-author","type":"issue"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("issue re-author: code=%d body=%s", w.Code, w.Body.String())
	}
	w := postRepoItemDecision(t, srv, 8, "writer", `{"decision":"re-author","type":"pr"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-author: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoItemDecisionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Decision != "re-author" || resp.Status != "adoption-queued" || resp.Label != delegatedecision.LabelAdoptSupersede || resp.AdmittedBy != "" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Result == nil || resp.Result.Action != rowActionAdopted || !strings.Contains(resp.Result.Outcome, "Re-authoring") {
		t.Fatalf("result = %+v", resp.Result)
	}
	if len(mock.merges) != 0 || strings.Join(mock.added, ",") != delegatedecision.LabelAdoptSupersede {
		t.Fatalf("re-author must only queue adoption: merges=%v added=%v", mock.merges, mock.added)
	}
	if len(mock.comments) != 1 || !strings.Contains(mock.comments[0], "re-author this pull request as a hive PR") || !strings.Contains(mock.comments[0], "Signed-off-by") {
		t.Fatalf("comments = %v", mock.comments)
	}
	if acts := decisionAuditActions(srv); len(acts) == 0 || acts[0] != auditRepoItemDecisionAdopt {
		t.Fatalf("audit = %v", acts)
	}
	if details := decisionAuditDetails(srv); len(details) == 0 || !strings.Contains(details[0], "decision=re-author") {
		t.Fatalf("audit detail = %v", details)
	}
}

func decisionAuditDetails(srv *Server) []string {
	var out []string
	for _, e := range srv.GetAudit().Recent(20) {
		out = append(out, e.Detail)
	}
	return out
}
