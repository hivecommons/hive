package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
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
// approved as it is; "Approve and let hive merge" would be the primary
// button under another name, so the one merge alternative is "Merge now
// as maintainer" — greyed with the reason when the operator lacks merge
// rights.
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
	if len(plan.Alternatives) == 0 || plan.Alternatives[0].Action != decisionActionMergeNow || plan.Alternatives[0].Label != decisionMergeNowLabel || !plan.Alternatives[0].Enabled {
		t.Fatalf("alternatives = %+v", plan.Alternatives)
	}
	for _, alt := range plan.Alternatives {
		if alt.Action == decisionActionQueueMerge {
			t.Fatalf("an approve verdict must not offer queue-merge twice: %+v", plan.Alternatives)
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
	// The lead merge alternative is "Approve and let hive merge"; merge-now
	// follows as the secondary.
	if len(plan.Alternatives) < 2 || plan.Alternatives[0].Action != decisionActionQueueMerge || plan.Alternatives[0].Label != "Approve and let hive merge" || plan.Alternatives[1].Action != decisionActionMergeNow {
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
	req, ok = operatorActionFromBody("decision", []byte(`{"decision":"merge-now","chosen":"Merge now as maintainer · hive would have adopt-supersede"}`))
	if !ok || req.Action != operatorActionMergeNow || req.Note != "Merge now as maintainer · hive would have adopt-supersede" {
		t.Fatalf("%+v %v", req, ok)
	}
}

// TestDelegatedPlanWhyAlreadyLandedNamesTheEvidence is
// hivecommons/hive#15732: the preview for #15544 said "Close #15544 as
// superseded by #15545" and then "Author @Karibhou is untrusted and their
// clanker is offline". A close driven by an empty diff and a merged
// landing says so, and never mentions the author's standing.
func TestDelegatedPlanWhyAlreadyLandedNamesTheEvidence(t *testing.T) {
	in := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15544, Author: "Karibhou", DiffEmpty: true,
		SupersededBy: &delegatedecision.MergedCloser{PR: 15545, Issue: 15543}}
	d := delegatedecision.Decision{Verdict: delegatedecision.VerdictCloseSuperseded, Rule: delegatedecision.RuleAlreadyFixed,
		Actions: []delegatedecision.Action{{Kind: delegatedecision.ActionCloseDuplicate, Target: 15545}}}
	if why := delegatedPlanWhy(in, d); why != "Diff is empty — the change already landed via #15545 (merged)." {
		t.Fatalf("why %q", why)
	}
	summary, _ := delegatedPlanWording(in, d)
	if summary != "Close #15544 as superseded by #15545." {
		t.Fatalf("summary %q", summary)
	}
	// Trusted or not, offline or not: the reason is the evidence.
	in.TrustedAuthor, in.ContributorOnline = true, false
	if why := delegatedPlanWhy(in, d); strings.Contains(why, "untrusted") || strings.Contains(why, "offline") {
		t.Fatalf("why mentions the author's standing: %q", why)
	}
	// Without the empty-diff fact the landing alone is the reason.
	in.DiffEmpty = false
	if why := delegatedPlanWhy(in, d); why != "The change already landed via #15545 (merged)." {
		t.Fatalf("why %q", why)
	}
	// Any other rule keeps the author's standing as the reason.
	other := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15029, Author: "Karibhou"}
	if why := delegatedPlanWhy(other, delegatedecision.Decision{Rule: "contributor-offline"}); why != "Author @Karibhou is untrusted and their clanker is offline." {
		t.Fatalf("why %q", why)
	}
}

// hivecommons/hive#15754: the plan for #15734 (an issue) said "Approve
// #15734 as it is and let the merge queue merge it" — PR wording. The
// default-approve plan branches on kind, and an advisory's plan is a
// close (all items resolved) or a no-write "keep open" (items remain).
func TestDelegatedPlanWordingIssueVsPRVsAdvisory(t *testing.T) {
	approve := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve"}
	for _, tc := range []struct {
		name         string
		in           delegatedecision.Input
		d            delegatedecision.Decision
		wantSummary  string
		wantNext     []string
		wantNotMatch string
	}{
		{"pr default-approve", delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 8}, approve,
			"Approve #8 as it is and let the merge queue merge it.",
			[]string{"Hive approves the current commit.", "#8 merges once CI is green and it has soaked."}, ""},
		{"issue default-approve", delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 15734}, approve,
			"Approve the direction of #15734 and let the hive pick it up.",
			[]string{"Hive records approval and removes the needs-decision hold.", "An agent claims #15734 on the next cycle."}, "merge"},
		{"advisory resolved", delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 15734, Advisory: &delegatedecision.AdvisoryChecklist{Resolved: 5, Total: 5}},
			delegatedecision.Decision{Verdict: delegatedecision.VerdictClose, Rule: delegatedecision.RuleAdvisoryResolved},
			"Close the advisory — its items are tracked by the hive.",
			[]string{"#15734 is closed as completed with a note that its items are resolved."}, ""},
		{"advisory in progress", delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 15734, Advisory: &delegatedecision.AdvisoryChecklist{Resolved: 2, Total: 5}},
			delegatedecision.Decision{Verdict: delegatedecision.VerdictRefused, Rule: delegatedecision.RuleAdvisoryInProgress, KeepsPark: true},
			"Keep open; hive is working 2/5 items.",
			[]string{"Nothing is written; the sweep ticks items as they close or merge.", "The advisory closes itself once every item is resolved."}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, next := delegatedPlanWording(tc.in, tc.d)
			if summary != tc.wantSummary {
				t.Fatalf("summary %q, want %q", summary, tc.wantSummary)
			}
			if strings.Join(next, "|") != strings.Join(tc.wantNext, "|") {
				t.Fatalf("consequences %q, want %q", next, tc.wantNext)
			}
			if tc.wantNotMatch != "" && strings.Contains(strings.ToLower(summary+" "+strings.Join(next, " ")), tc.wantNotMatch) {
				t.Fatalf("issue wording mentions %q: %q %q", tc.wantNotMatch, summary, next)
			}
		})
	}
}

// End to end on the plan endpoint: issue #7 as a dedupe checklist with
// items open plans a no-write keep-open; with every item ticked it plans
// a close; as an ordinary parked issue it plans the issue approval.
func TestDelegatedDecisionPlanIssueAndAdvisory(t *testing.T) {
	checklist := "<!-- hive:dedupe-checklist repo=testorg/testrepo -->\nDecision required: yes\n\n- [x] #1 — close as superseded by #4\n- [ ] #2 — close as superseded by #5\n"
	for _, tc := range []struct {
		name      string
		body      string
		labels    []string
		wantWould string
		wantRule  string
		wantSum   string
	}{
		{"advisory in progress", checklist, []string{"agent/dedupe", "stage/045-needs-decision"}, string(delegatedecision.VerdictRefused), delegatedecision.RuleAdvisoryInProgress, "Keep open; hive is working 1/2 items."},
		{"advisory resolved", strings.ReplaceAll(checklist, "- [ ]", "- [x]"), []string{"agent/dedupe"}, string(delegatedecision.VerdictClose), delegatedecision.RuleAdvisoryResolved, "Close the advisory — its items are tracked by the hive."},
		{"ordinary issue", "Please add a flag.", []string{"stage/045-needs-decision"}, string(delegatedecision.VerdictApprove), "default-approve", "Approve the direction of #7 and let the hive pick it up."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &repoItemDecisionMock{issueTitle: "triage: duplicates", issueBody: tc.body, issueLabels: tc.labels}
			srv := newDelegatedServer(t, mock)
			w := postDelegatedPlan(t, srv, 7, "writer", `{"type":"issue"}`, map[string]string{"X-Hive-Role": "merger"})
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var plan delegatedDecisionPlan
			if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Would != tc.wantWould || plan.Rule != tc.wantRule || plan.Summary != tc.wantSum || plan.Writes {
				t.Fatalf("plan would=%q rule=%q summary=%q writes=%v", plan.Would, plan.Rule, plan.Summary, plan.Writes)
			}
			if len(mock.comments) != 0 || len(mock.edits) != 0 {
				t.Fatalf("a plan may not write: %v %v", mock.comments, mock.edits)
			}
		})
	}
}

// hivecommons/hive#15722, the live case: a clanker's PR the hive adopted in
// place (contributor-lane:adopted-by-hive), parked at 085 by the reviewer's
// requires_human / high-risk marker, every check green and only the tide
// context pending (mergeable_state "unstable"). The preview said "Send
// #15722 back to its author with one instruction: make CI green" / "CI is
// red". It must read the recorded park reason instead, never call a
// pending-only head red, and — the hive being the author now — hand the
// finding to the hive's own fixer.
func TestDelegatedDecisionPlanAdoptedRequiresHumanIsFixersNotCI(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prMergeState: "unstable", prTidePending: true, prLabels: []string{"stage/085-needs-decision"}}
	srv := newDelegatedServer(t, mock)
	srv.stageStore = stagelabels.NewStore("", time.Now)
	key := stagelabels.Key("testorg/testrepo", 8)
	srv.stageStore.Observe(key, stagelabels.NeedsDecision, stageengine.StageReasonVerdictRequiresHuman)
	srv.stageStore.MarkAdoptedInPlace(key, "feedfacefeedfacefeedfacefeedfacefeedface", "contributor-offline", time.Now())

	// The tracker row's words, as the SPA sends them.
	body := `{"type":"pr","reason":"reviewer-verdict:requires_human","caption":"the reviewer marked this change high-risk"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("summary: %s\nwhy: %s\nconsequences: %v", plan.Summary, plan.Why, plan.Consequences)
	if plan.Would != string(delegatedecision.VerdictDirection) || plan.Rule != delegatedecision.RuleAdoptedFixer {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.HasPrefix(plan.Summary, "Have hive's fixer address the reviewer's finding") || !strings.Contains(plan.Summary, "no action needed from you") {
		t.Fatalf("summary %q", plan.Summary)
	}
	if !strings.Contains(plan.Why, "requires_human") {
		t.Fatalf("why %q does not name the recorded park reason", plan.Why)
	}
	for _, text := range append([]string{plan.Summary, plan.Why}, plan.Consequences...) {
		lower := strings.ToLower(text)
		if strings.Contains(text, "CI") || strings.Contains(lower, "back to its author") {
			t.Errorf("a green head parked on requires_human is described as red or returned: %q", text)
		}
	}
	if len(mock.comments) != 0 {
		t.Fatalf("a plan may not write: %v", mock.comments)
	}

	// The same head, not adopted: the clanker's PR is not "red" either —
	// the plan never says CI is red of a head with no failing check.
	mock2 := &repoItemDecisionMock{prForeign: true, prMergeState: "unstable", prTidePending: true, prLabels: []string{"stage/085-needs-decision"}}
	srv2 := newDelegatedServer(t, mock2)
	srv2.stageStore = stagelabels.NewStore("", time.Now)
	srv2.stageStore.Observe(key, stagelabels.NeedsDecision, stageengine.StageReasonVerdictRequiresHuman)
	w = postDelegatedPlan(t, srv2, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan2 delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan2); err != nil {
		t.Fatal(err)
	}
	t.Logf("not adopted: summary: %s\nwhy: %s", plan2.Summary, plan2.Why)
	for _, text := range append([]string{plan2.Summary, plan2.Why}, plan2.Consequences...) {
		if strings.Contains(text, "CI is red") || strings.Contains(text, "make CI green") {
			t.Errorf("not adopted: head with green checks called red: %q", text)
		}
	}
}

// delegatedPlanInstruction / delegatedPlanBlockers: pending checks are not
// red, and the recorded reviewer finding is the WHY.
func TestDelegatedPlanPendingCIIsNotRed(t *testing.T) {
	in := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15722, Author: "clubanderson", OwnerAuthor: true, TrustedAuthor: true, CIGreen: false, CIPending: true, Conflicting: true}
	d := delegatedecision.Decide(in)
	if d.Verdict != delegatedecision.VerdictDirection {
		t.Fatalf("verdict %s rule %s", d.Verdict, d.Rule)
	}
	summary, _ := delegatedPlanWording(in, d)
	if summary != "Send #15722 back to its author with one instruction: rebase onto the base branch, nothing else." {
		t.Fatalf("summary %q", summary)
	}
	if why := delegatedPlanWhy(in, d); why != "It conflicts with the base branch." {
		t.Fatalf("why %q", why)
	}
	in.Conflicting, in.ReviewerRequiresHuman, in.ReviewerFinding = false, true, "the reviewer marked this change high-risk"
	in.AdoptedByHive, in.OwnerAuthor, in.TrustedAuthor, in.Author = true, false, false, "mendezr"
	d = delegatedecision.Decide(in)
	if d.Rule != delegatedecision.RuleAdoptedFixer {
		t.Fatalf("verdict %s rule %s", d.Verdict, d.Rule)
	}
	summary, next := delegatedPlanWording(in, d)
	if summary != "Have hive's fixer address the reviewer's finding (the reviewer marked this change high-risk) on #15722; no action needed from you." {
		t.Fatalf("summary %q", summary)
	}
	if why := delegatedPlanWhy(in, d); why != "The reviewer returned requires_human: the reviewer marked this change high-risk." {
		t.Fatalf("why %q", why)
	}
	if len(next) != 2 {
		t.Fatalf("then items %v", next)
	}
	if delegatedPlanMergeableAsIs(in, d) {
		t.Fatal("a head with checks still running is not mergeable as it is")
	}
}
