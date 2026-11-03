package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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
	if stageNumberClaimed != stagelabels.Claimed.Number() || stageNumberPRInProgress != stagelabels.PRInProgress.Number() {
		t.Fatalf("band ordinals drifted from the stage table: %d/%d", stagelabels.Claimed.Number(), stagelabels.PRInProgress.Number())
	}
	// Covered is 040 plus every PR stage an older build may have mirrored
	// onto the issue. The parking stages (045, and 085 — the pre-045 issue
	// spelling of the park) sort past 040 but are NOT coverage: an issue
	// there waits on a person, and no PR need exist. Neither is the
	// prior-art park 017, which sorts before 020 claimed.
	notCovered := map[stagelabels.Stage]bool{
		stagelabels.Actionable:         true,
		stagelabels.TriageNeeded:       true,
		stagelabels.PossiblyExisting:   true,
		stagelabels.Claimed:            true,
		stagelabels.PRInProgress:       true,
		stagelabels.IssueNeedsDecision: true,
		stagelabels.NeedsDecision:      true,
	}
	for _, d := range stagelabels.Table {
		if got := stageNumber(d.Stage.Label()); got != d.Number {
			t.Errorf("stageNumber(%s) = %d, want %d", d.Stage.Label(), got, d.Number)
		}
		if want := !notCovered[d.Stage]; issueStageCovered([]string{d.Stage.Label()}) != want {
			t.Errorf("issueStageCovered(%s) = %v, want %v", d.Stage.Label(), !want, want)
		}
	}
	// The retired 090 spelling is the PR stage ready-for-human-merge, so on
	// an issue it is a mirrored PR stage like any other.
	if !issueStageCovered([]string{"stage/090-needs-human"}) {
		t.Error("retired stage/090-needs-human (a PR stage) must still read as covered")
	}
	if !issueStageCovered([]string{stagelabels.IssueNeedsDecision.Label(), stagelabels.CoveredByPR.Label()}) {
		t.Error("040 next to a stale park label must still read as covered")
	}
	for _, l := range []string{stagelabels.StalledLabel, stagelabels.RegressedLabel, "stage/", "stage/x-y", "role/trusted", "bug"} {
		if stageNumber(l) != 0 || issueStageCovered([]string{l}) {
			t.Errorf("%q must not parse as a stage ordinal", l)
		}
	}
}

// A parked issue (stage/045-needs-decision, or the pre-045 stage/085
// spelling) waits on a person: it files under Parked with no "already done"
// tick, whatever its number. Before this, the ordinal test (>= 040) put
// every parked issue in Confirm & close with a ✓ already done signal.
func TestIssueBandParkingStageIsNotDone(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		labels   []string
		want     string
		wantTick bool
	}{
		{"045 alone is parked", []string{stagelabels.IssueNeedsDecision.Label()}, "waiting", false},
		{"045 + needs-human is parked", []string{"needs-human", stagelabels.IssueNeedsDecision.Label(), "kind/feature"}, "waiting", false},
		{"pre-045 085 spelling is parked", []string{stagelabels.NeedsDecision.Label()}, "waiting", false},
		{"040 covered-by-pr is done", []string{stagelabels.CoveredByPR.Label()}, "done", true},
		{"mirrored PR stage is done", []string{stagelabels.ReviewPending.Label()}, "done", true},
		{"045 + hive/already-done is still done", []string{stagelabels.IssueNeedsDecision.Label(), "hive/already-done"}, "done", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := IssueBand(github.Issue{Repo: "octo/demo", Number: 82, Labels: tt.labels}, false, config.DashboardIssueBandsConfig{}, now)
			if info.Band != tt.want {
				t.Fatalf("band = %q, want %q (info=%+v)", info.Band, tt.want, info)
			}
			ticked := false
			for _, sig := range info.Signals {
				if sig.Label == "already done" {
					ticked = true
				}
			}
			if ticked != tt.wantTick {
				t.Fatalf("already-done signal = %v, want %v (signals=%+v)", ticked, tt.wantTick, info.Signals)
			}
		})
	}
}
