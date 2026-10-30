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
	if plan.Summary != "Re-open #8 as hive's own PR with the same commits and merge that; close #8 as superseded." {
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
	// The ladder landed on contributor-offline, not mergeable-as-is, so
	// the preview must not offer to merge it as it is (#15332's dialog
	// said "not mergeable" and offered the merge anyway).
	if plan.Ladder == nil || plan.Ladder.Decision != delegatedecision.RuleContributorOffline || plan.MergeableAsIs {
		t.Fatalf("ladder=%+v mergeable_as_is=%v", plan.Ladder, plan.MergeableAsIs)
	}
	for _, alt := range plan.Alternatives {
		if alt.Action == decisionActionMergeNow {
			t.Fatalf("merge-now offered on a head the ladder ruled not mergeable: %+v", plan.Alternatives)
		}
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
}

// The mergeable-as-is rung: a green, conflict-free contributor head is
// approved as it is, and "Approve & merge as-is" is offered — greyed with
// the reason when the operator lacks merge rights.
func TestDelegatedDecisionPlanMergeableAsIsOffersMerge(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	body := `{"type":"pr","reason":"needs-decision","caption":"needs a decision"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Would != string(delegatedecision.VerdictApprove) || plan.Ladder == nil || plan.Ladder.Decision != delegatedecision.RuleMergeableAsIs || !plan.MergeableAsIs {
		t.Fatalf("plan = %+v ladder=%+v", plan, plan.Ladder)
	}
	if plan.Summary != "Approve #8 as it is and let the merge queue merge it." || strings.Contains(plan.Summary, "Hive would") {
		t.Fatalf("summary %q", plan.Summary)
	}
	if len(plan.Alternatives) == 0 || plan.Alternatives[0].Action != decisionActionMergeNow || plan.Alternatives[0].Label != delegatedPlanAlternativeLabel || !plan.Alternatives[0].Enabled {
		t.Fatalf("alternatives = %+v", plan.Alternatives)
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

// hivecommons/hive#15332's preview, as the operator saw it: a direction
// on a red head. The sentence is verb first with no "Hive would:" prefix
// (the dialog composed it twice), the why is one short line, there are at
// most two "then" items and nothing visible uses hive's vocabulary.
func TestDelegatedPlanWordingDirectionIsOneSentence(t *testing.T) {
	in := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15332, Author: "clubanderson", OwnerAuthor: true, TrustedAuthor: true, CIGreen: false}
	d := delegatedecision.Decide(in)
	if d.Verdict != delegatedecision.VerdictDirection {
		t.Fatalf("verdict %s rule %s", d.Verdict, d.Rule)
	}
	summary, next := delegatedPlanWording(in, d)
	if summary != "Send #15332 back to its author with one instruction: make CI green, nothing else." {
		t.Fatalf("summary %q", summary)
	}
	why := delegatedPlanWhy(in, d)
	if why != "CI is red." {
		t.Fatalf("why %q", why)
	}
	if n := len(strings.Fields(why)); n > 12 {
		t.Fatalf("why is %d words: %q", n, why)
	}
	if len(next) != 2 {
		t.Fatalf("then items %v", next)
	}
	for _, text := range append([]string{summary, why}, next...) {
		for _, jargon := range []string{"Hive would", "park", "audit marker", "stage", "ladder", "/hive decision"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(jargon)) {
				t.Errorf("%q leaks %q", text, jargon)
			}
		}
	}
	if delegatedPlanMergeableAsIs(in, d) {
		t.Fatal("a red head is not mergeable as it is")
	}
	// Conflicts and a missing sign-off join the one instruction.
	in.Conflicting, in.DCOMissing = true, true
	if s, _ := delegatedPlanWording(in, delegatedecision.Decide(in)); s != "Send #15332 back to its author with one instruction: rebase onto the base branch, add the sign-off, make CI green, nothing else." {
		t.Fatalf("summary %q", s)
	}
	// Every verdict's then-list fits the two-item budget.
	for _, v := range []delegatedecision.Verdict{delegatedecision.VerdictAdoptSupersede, delegatedecision.VerdictApprove, delegatedecision.VerdictCloseSuperseded, delegatedecision.VerdictRefused, delegatedecision.VerdictOwnerOnly, delegatedecision.VerdictRecommendOnly, delegatedecision.VerdictOperatorAction, delegatedecision.VerdictNotNow} {
		s, n := delegatedPlanWording(in, delegatedecision.Decision{Verdict: v, Rationale: "because (hivecommons/hive#1); and more", Text: "do the thing (hivecommons/hive#2)"})
		if len(n) > 2 || strings.Contains(s, "Hive would") || strings.Contains(s, "#1)") || strings.Contains(s, "#2)") {
			t.Errorf("%s: summary %q then %v", v, s, n)
		}
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
	if plan.Would != string(delegatedecision.VerdictRecommendOnly) || !strings.HasPrefix(plan.Summary, "Recommend only: ") {
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
