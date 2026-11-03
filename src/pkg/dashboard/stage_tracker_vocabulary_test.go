package dashboard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// rawStageCodeRE is what no operator-facing string may contain: a stage
// label or a bare "060 reviewed" code.
var rawStageCodeRE = regexp.MustCompile(`stage/\d{3}|\b0\d{2} [a-z-]+\b|\b0\d{2}-[a-z-]+\b`)

// Every label in stagelabels has a column and a phrase, in the chevron's
// vocabulary (hivecommons/hive#15096 roll 3: 060 reviewed was drawn at the
// merge gate while the event line said "PR at 060 reviewed").
func TestStageTrackerVocabularyTable(t *testing.T) {
	want := map[stagelabels.Stage]struct {
		column int
		phrase string
	}{
		stagelabels.Actionable:             {trackerColumnActionable, "actionable"},
		stagelabels.TriageNeeded:           {trackerColumnActionable, "waiting for triage"},
		stagelabels.PossiblyExisting:       {trackerColumnActionable, "waiting for the author to confirm the gap (prior art found)"},
		stagelabels.Claimed:                {trackerColumnClaimed, "claimed"},
		stagelabels.PRInProgress:           {trackerColumnPR, "a PR in progress"},
		stagelabels.CoveredByPR:            {trackerColumnPR, "covered by a PR"},
		stagelabels.IssueNeedsDecision:     {trackerColumnActionable, "waiting for a human decision"},
		stagelabels.ReviewPending:          {trackerColumnReview, "in CI / review"},
		stagelabels.Reviewed:               {trackerColumnReview, "review passed · waiting for merge admission"},
		stagelabels.Fixing:                 {trackerColumnReview, "being fixed by hive"},
		stagelabels.NeedsAuthor:            {trackerColumnReview, "waiting for its author"},
		stagelabels.ReturnedToContributor:  {trackerColumnReview, "returned to the contributor"},
		stagelabels.AdoptedByHive:          {trackerColumnReview, "adopted by hive"},
		stagelabels.AwaitingMergeAdmission: {trackerColumnMergeGate, "at the merge queue"},
		stagelabels.NeedsDecision:          {trackerColumnMergeGate, "waiting for a human decision"},
		stagelabels.ReadyForHumanMerge:     {trackerColumnMergeGate, "ready for human merge"},
		stagelabels.PreMergeSoak:           {trackerColumnMergeGate, "soaking before merge"},
		stagelabels.MergeQueue:             {trackerColumnMergeGate, "in the merge queue"},
	}
	for _, d := range stagelabels.Table {
		w, ok := want[d.Stage]
		if !ok {
			t.Errorf("%s: no column / phrase in the vocabulary table", d.Stage)
			continue
		}
		if got := stageTrackerColumn(d.Stage); got != w.column {
			t.Errorf("%s: column %d, want %d", d.Stage, got, w.column)
		}
		if got := stageTrackerStagePhrase(d.Stage); got != w.phrase {
			t.Errorf("%s: phrase %q, want %q", d.Stage, got, w.phrase)
		}
		if rawStageCodeRE.MatchString(stageTrackerShort(d.Stage)) {
			t.Errorf("%s: chevron word %q carries a stage code", d.Stage, stageTrackerShort(d.Stage))
		}
	}
	if stageTrackerColumn(stagelabels.Reviewed) == trackerColumnMergeGate {
		t.Fatal("060 reviewed is pre-admission and must not be drawn at the merge gate")
	}
}

// Rendered event lines never carry a stage code, for every stage a lane
// reason can name, and the cover line says "covers" once.
func TestStageTrackerEventLinesUseChevronVocabulary(t *testing.T) {
	for _, d := range stagelabels.Table {
		reason := coverReason(15118, d.Stage)
		caption, _ := stageTrackerCaption(stagelabels.CoveredByPR, reason, "", "issue", false, stagelabels.Record{}, false, 15118, "")
		if rawStageCodeRE.MatchString(caption) {
			t.Errorf("%s: event line %q carries a stage code", d.Stage, caption)
		}
		if strings.Count(strings.ToLower(caption), "covers") > 1 {
			t.Errorf("%s: event line %q repeats itself", d.Stage, caption)
		}
		if d.PR && !strings.Contains(caption, stageTrackerStagePhrase(d.Stage)) {
			t.Errorf("%s: event line %q does not say what the PR is doing", d.Stage, caption)
		}
		for _, raw := range []string{"moved to " + d.Stage.Label(), "PR at " + d.Stage.String(), "label " + d.Stage.Label()} {
			if got := humanizeStageReason(raw); rawStageCodeRE.MatchString(got) {
				t.Errorf("humanizeStageReason(%q) = %q carries a stage code", raw, got)
			}
		}
	}
	got, _ := stageTrackerCaption(stagelabels.CoveredByPR, coverReason(15118, stagelabels.Reviewed), "", "issue", false, stagelabels.Record{}, false, 15118, "")
	if want := "An open PR #15118 covers this issue — its PR is review passed · waiting for merge admission"; got != want {
		t.Fatalf("caption %q, want %q", got, want)
	}
}
