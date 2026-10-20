package dashboard

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An issue still carrying needs-triage reports the needs-triage modifier so
// the activity monitor's action chip can read "triage needed".
func TestStageTrackerModifiers_NeedsTriage(t *testing.T) {
	got := stageTrackerModifiers([]string{"needs-triage", "bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false)
	if !slices.Equal(got, []string{modNeedsTriage}) {
		t.Fatalf("modifiers = %v, want [%s]", got, modNeedsTriage)
	}
	if got := stageTrackerModifiers([]string{"bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false); len(got) != 0 {
		t.Fatalf("modifiers = %v, want none", got)
	}
}

// An issue at 015-triage-needed lights the Actionable column with its own
// word, so the redundant needs-triage chip is dropped there (as the
// needs-decision chip is on 045); on any other stage the chip stays. The
// caption names the maintainer as the next mover (hivecommons/hive#13268).
func TestStageTracker_TriageNeededStage(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 35, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	store.Observe(stagelabels.Key("acme/console", 21), stagelabels.TriageNeeded, "triage needed: filed by the hive (hive), no maintainer triage label")
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 21, Title: "Review backlog (security)", Author: "hive-app[bot]", Labels: []string{"stage/015-triage-needed", "needs-triage", "agent/scanner"}},
			github.Issue{Number: 22, Title: "Held by the gate", Author: "newcomer", AuthorIsHuman: true, Labels: []string{"stage/020-claimed", "needs-triage"}},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)
	issue21 := trackerItem(t, resp, "issue", 21)
	if issue21.Stage != trackerNumberTriageNeeded || issue21.StageLabel != stagelabels.TriageNeeded.Label() || issue21.Mirrored {
		t.Fatalf("issue21 = %+v", issue21)
	}
	if hasMod(issue21, modNeedsTriage) {
		t.Fatalf("015 must drop the redundant needs-triage chip: %v", issue21.Modifiers)
	}
	if issue21.Actor != "maintainer" || !strings.HasPrefix(issue21.Caption, "Actionable, but no maintainer has looked at it yet; waiting for triage") {
		t.Fatalf("issue21 caption=%q actor=%q", issue21.Caption, issue21.Actor)
	}
	if !strings.Contains(issue21.Caption, "filed by the hive") {
		t.Fatalf("caption %q must carry the derivation reason", issue21.Caption)
	}
	issue22 := trackerItem(t, resp, "issue", 22)
	if issue22.Stage != trackerNumberClaimed || !hasMod(issue22, modNeedsTriage) {
		t.Fatalf("needs-triage chip must survive on other stages: %+v", issue22)
	}
	var short string
	for _, st := range resp.Stages {
		if st.Number == trackerNumberTriageNeeded {
			short = st.Short
		}
	}
	if short != "Triage needed" {
		t.Fatalf("stage 015 short = %q", short)
	}
}
