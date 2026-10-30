package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15810: a PR the merge queue holds because an open PR in
// another lane claims the same issue reads "claim-conflict:<other>" on the
// gate, rides no train, captions "held · duplicate of #<other> · needs
// decision", and its decision options name both PRs — never "all N PRs".
func TestStageTrackerApplyClaimConflict(t *testing.T) {
	reason := prstage.MergeTrain{State: prstage.MergeTrainStateClaimConflict, Front: 15793, Position: 15773}.Reason()
	it := StageTrackerItem{Repo: "hivecommons/hive", Kind: "pr", Number: 15783, Stage: 85, BaseRef: "v5", Reason: reason}
	stageTrackerApplyMergeTrain(&it)
	if it.MergeTrain != nil {
		t.Fatalf("a held claimant rides no train: %+v", it.MergeTrain)
	}
	if it.MergeGate == nil || it.MergeGate.State != "claim-conflict:15793" || it.MergeGate.DuplicateOf != 15793 || it.MergeGate.DuplicateIssue != 15773 {
		t.Fatalf("gate = %+v", it.MergeGate)
	}
	if !slices.Contains(it.Modifiers, modNeedsDecision) {
		t.Fatalf("modifiers = %v", it.Modifiers)
	}
	if c := stageTrackerClaimConflictCaption(&it); c != "held · duplicate of #15793 · needs decision" {
		t.Fatalf("caption = %q", c)
	}
	d := stageTrackerDecisionNeededAt(it, nil, "", time.Now())
	if d == nil || !strings.Contains(d.Question, "#15783 and #15793 both claim issue #15773") {
		t.Fatalf("decision = %+v", d)
	}
	labels := make([]string, 0, len(d.Options))
	for _, o := range d.Options {
		labels = append(labels, o.Label)
	}
	if !slices.Contains(labels, "Keep #15783 — admit it to the merge queue") || !slices.Contains(labels, "Close #15783 as superseded by #15793") {
		t.Fatalf("options = %v", labels)
	}
	for _, l := range labels {
		if strings.Contains(l, "all 2") {
			t.Fatalf("generic option %q", l)
		}
	}

	// The other row — a plain duplicate-of hold — is untouched by the
	// claim-conflict caption.
	dup := StageTrackerItem{Kind: "pr", Number: 15407, Reason: prstage.MergeTrain{State: prstage.MergeTrainStateDuplicateOf, Front: 15402, Position: 15396}.Reason()}
	stageTrackerApplyMergeTrain(&dup)
	if c := stageTrackerClaimConflictCaption(&dup); c != "" {
		t.Fatalf("duplicate-of caption = %q", c)
	}
}

// The delegate's plan for a held claimant names the concrete PRs and the
// two ways out instead of "all 2 PRs", and only recommends.
func TestDelegatedPlanWordingClaimConflictNamesPRs(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	store := stagelabels.NewStore("", func() time.Time { return now })
	store.Observe(stagelabels.Key("hivecommons/hive", 15783), stagelabels.NeedsDecision,
		prstage.MergeTrain{State: prstage.MergeTrainStateClaimConflict, Front: 15793, Position: 15773}.Reason())
	other, issue := delegatedClaimConflict(store, "hivecommons/hive", 15783)
	if other != 15793 || issue != 15773 {
		t.Fatalf("store read = #%d issue #%d", other, issue)
	}
	if w, _ := delegatedTrainDuplicate(store, "hivecommons/hive", 15783); w != 0 {
		t.Fatal("a claim conflict is not the duplicate-of pick")
	}

	in := delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 15783, HiveAuthored: true, CIGreen: true, ClaimConflictWith: other, ClaimConflictIssue: issue}
	d := delegatedecision.Decide(in)
	if d.Verdict != delegatedecision.VerdictRecommendOnly || d.Rule != delegatedecision.RuleSameIssueClaimConflict {
		t.Fatalf("verdict %s rule %s", d.Verdict, d.Rule)
	}
	summary, next := delegatedPlanWording(in, d)
	if summary != "Hold both: #15783 and #15793 claim issue #15773. Keep #15793, close #15783 — or keep #15783, close #15793; the hive will not pick." {
		t.Fatalf("summary %q", summary)
	}
	if len(next) != 2 || strings.Contains(summary, "all 2") {
		t.Fatalf("then %v", next)
	}
}
