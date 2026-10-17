package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The claimed / pr-in-progress stage labels (and, for the migration cycle,
// the retired hive/in-progress and claimed) are the agent-start signal; the
// band logic must file them with the other claimed markers so a started
// issue reads as in-progress, while covered-by-pr and the mirrored PR
// stages read as done.
func TestIssueClaimed_StageLabels(t *testing.T) {
	cases := map[string]struct {
		labels []string
		want   bool
	}{
		"stage-claimed":        {[]string{stagelabels.Claimed.Label()}, true},
		"stage-pr-in-progress": {[]string{stagelabels.PRInProgress.Label()}, true},
		"legacy-in-progress":   {[]string{"hive/in-progress"}, true},
		"upper":                {[]string{"HIVE/In-Progress"}, true},
		"legacy-claimed":       {[]string{"claimed"}, true},
		"claimed-by":           {[]string{"hive/claimed-by-scanner"}, true},
		"stage-actionable":     {[]string{stagelabels.Actionable.Label()}, false},
		"stage-covered":        {[]string{stagelabels.CoveredByPR.Label()}, false},
		"covered-by-pr":        {[]string{"hive/covered-by-pr"}, false},
		"covered-by-pr-bee":    {[]string{"🐝 covered by PR"}, false},
		"health-only":          {[]string{stagelabels.StalledLabel}, false},
		"none":                 {nil, false},
	}
	for name, tc := range cases {
		if got := issueClaimed(tc.labels); got != tc.want {
			t.Errorf("%s: issueClaimed(%v) = %v, want %v", name, tc.labels, got, tc.want)
		}
	}
}

func TestIssueStageCoveredAndOrdinalsMatchTable(t *testing.T) {
	if stageNumberClaimed != stagelabels.Claimed.Number() || stageNumberPRInProgress != stagelabels.PRInProgress.Number() || stageNumberCoveredByPR != stagelabels.CoveredByPR.Number() {
		t.Fatalf("band ordinals drifted from the stage table: %d/%d/%d", stagelabels.Claimed.Number(), stagelabels.PRInProgress.Number(), stagelabels.CoveredByPR.Number())
	}
	for _, d := range stagelabels.Table {
		if got := stageNumber(d.Stage.Label()); got != d.Number {
			t.Errorf("stageNumber(%s) = %d, want %d", d.Stage.Label(), got, d.Number)
		}
		if want := d.Number >= stagelabels.CoveredByPR.Number(); issueStageCovered([]string{d.Stage.Label()}) != want {
			t.Errorf("issueStageCovered(%s) = %v, want %v", d.Stage.Label(), !want, want)
		}
	}
	for _, l := range []string{stagelabels.StalledLabel, stagelabels.RegressedLabel, "stage/", "stage/x-y", "role/trusted", "bug"} {
		if stageNumber(l) != 0 || issueStageCovered([]string{l}) {
			t.Errorf("%q must not parse as a stage ordinal", l)
		}
	}
}
