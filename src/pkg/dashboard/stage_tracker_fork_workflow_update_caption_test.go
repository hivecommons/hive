package dashboard

import (
	"strings"
	"testing"

	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// fork-workflow-update (GitHub refused the App's update-branch on a fork
// head because the base changed workflow files) reads in words, at 072 for
// an agent's PR and at 085 for a person's, not as "fork workflow update".
func TestStageTrackerCaptionForkWorkflowUpdate(t *testing.T) {
	const want = "base changed workflow files the hive cannot push to a fork; the branch needs the base merged in"
	if got := humanizeStageReason(stageengineprstage.AutoMergeReasonForkWorkflowUpdate); got != want {
		t.Fatalf("humanizeStageReason = %q, want %q", got, want)
	}
	for _, s := range []string{"\u2014", "\u2013", "@", "rebase"} {
		if strings.Contains(want, s) {
			t.Fatalf("caption clause carries %q", s)
		}
	}
	for _, tc := range []struct {
		stage stagelabels.Stage
		lead  string
	}{
		{stagelabels.ReturnedToContributor, "Returned to @mallory"},
		{stagelabels.NeedsDecision, "A human decision is owed"},
	} {
		caption, _ := stageTrackerCaption(tc.stage, stageengineprstage.AutoMergeReasonForkWorkflowUpdate, "mallory", "pr", false, stagelabels.Record{}, false, 0, "aaa")
		if !strings.HasPrefix(caption, tc.lead) || !strings.Contains(caption, want) {
			t.Fatalf("stage %s caption = %q, want %q with %q", tc.stage, caption, tc.lead, want)
		}
	}
}
