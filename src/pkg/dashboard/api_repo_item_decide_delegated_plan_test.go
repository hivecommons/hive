package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

func postDelegatedPlan(t *testing.T, srv *Server, number int, user, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/"+strconv.Itoa(number)+"/decide-delegated/plan", strings.NewReader(body))
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

// hivecommons/hive#15126: "Let hive decide" previews before acting. The
// plan names the verdict in plain words with its consequences and the
// alternatives, and writes nothing.
func TestDelegatedDecisionPlanPreviewsAdoptWithoutWrites(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	body := `{"type":"pr","reason":"adoption-stalled:contributor-offline","caption":"adoption stalled"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("summary: %s\nwhy: %s\nconsequences: %v", plan.Summary, plan.Why, plan.Consequences)
	if plan.Would != string(delegatedecision.VerdictAdoptSupersede) || plan.Writes {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Summary != "Hive would: re-author #8 as a hive PR with the same commits and merge it; #8 will be closed as superseded." {
		t.Fatalf("summary %q", plan.Summary)
	}
	// The mock grants @someone write, so only the offline half applies here.
	if plan.Why != "Their clanker is offline." {
		t.Fatalf("why %q", plan.Why)
	}
	untrusted := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15029, Author: "Karibhou"}
	if why := delegatedPlanWhy(untrusted, delegatedecision.Decision{Rule: "contributor-offline"}); why != "Author @Karibhou is untrusted and their clanker is offline." {
		t.Fatalf("why %q", why)
	}
	if plan.Acting != "Adopting · opening hive copy…" || len(plan.Consequences) < 2 {
		t.Fatalf("acting %q consequences %v", plan.Acting, plan.Consequences)
	}
	if len(plan.Alternatives) == 0 || plan.Alternatives[0].Action != decisionActionMergeNow || plan.Alternatives[0].Label != delegatedPlanAlternativeLabel || !plan.Alternatives[0].Enabled {
		t.Fatalf("alternatives = %+v", plan.Alternatives)
	}
	if len(mock.comments) != 0 || len(mock.edits) != 0 || len(mock.added) != 0 || len(mock.removed) != 0 || len(mock.reviews) != 0 || len(mock.merges) != 0 {
		t.Fatalf("a plan may not write: comments=%v edits=%v added=%v removed=%v reviews=%v merges=%v", mock.comments, mock.edits, mock.added, mock.removed, mock.reviews, mock.merges)
	}
	if _, found := srv.delegatedJobs.get(delegatedJobKey("testorg/testrepo", 8)); found {
		t.Fatal("a plan must not start a job")
	}
	if srv.operatorActionStore != nil {
		if acts := srv.operatorActionStore.List("testorg/testrepo", 8); len(acts) != 0 {
			t.Fatalf("a plan must not reach the ledger: %+v", acts)
		}
	}

	// Without the merger role the as-is merge is greyed with the reason.
	w = postDelegatedPlan(t, srv, 8, "writer", body, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Alternatives[0].Enabled || plan.Alternatives[0].ReasonIfDisabled != decisionWhyMergerRole {
		t.Fatalf("alternatives = %+v", plan.Alternatives)
	}
}

func TestDelegatedDecisionPlanReadyForHumanMergeUsesShortcut(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/090-ready-for-human-merge", "role/newcomer"}}
	srv := newDelegatedServer(t, mock)
	body := `{"type":"pr","reason":"contributor-lane:author-association","caption":"Ready for human merge · untrusted author"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Would != string(delegatedecision.VerdictRecommendOnly) || !strings.HasPrefix(plan.Summary, "Hive would: recommend only") {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Alternatives[0].Action != decisionActionMergeNow || plan.Alternatives[0].Label != delegatedPlanAlternativeLabel {
		t.Fatalf("alternatives = %+v", plan.Alternatives)
	}
	if len(mock.comments) != 0 || len(mock.added) != 0 {
		t.Fatalf("a plan may not write: %v %v", mock.comments, mock.added)
	}
}

func TestDelegatedDecisionPlanAuth(t *testing.T) {
	srv := newDelegatedServer(t, &repoItemDecisionMock{prForeign: true})
	srv.deps.Config.Decisions.Delegate.Enabled = boolPtr(false)
	if w := postDelegatedPlan(t, srv, 8, "writer", `{"type":"pr"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("disabled: code=%d", w.Code)
	}
}

// The preview button pressed is kept on the ledger as the action's note.
func TestOperatorActionFromBodyRecordsChosenButton(t *testing.T) {
	req, ok := operatorActionFromBody("decide-delegated", []byte(`{"type":"pr","chosen":"Let hive do that · previewed adopt-supersede"}`))
	if !ok || req.Action != operatorActionLetHiveDecide || req.Note != "Let hive do that · previewed adopt-supersede" {
		t.Fatalf("%+v %v", req, ok)
	}
	req, ok = operatorActionFromBody("decision", []byte(`{"decision":"merge-now","chosen":"Approve & merge as-is · hive would have adopt-supersede"}`))
	if !ok || req.Action != operatorActionMergeNow || req.Note != "Approve & merge as-is · hive would have adopt-supersede" {
		t.Fatalf("%+v %v", req, ok)
	}
}
