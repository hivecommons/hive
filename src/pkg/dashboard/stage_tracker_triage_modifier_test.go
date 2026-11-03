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
	got, _ := stageTrackerModifiers([]string{"needs-triage", "bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false, "", "issue", stagelabels.Actionable, false)
	if !slices.Equal(got, []string{modNeedsTriage}) {
		t.Fatalf("modifiers = %v, want [%s]", got, modNeedsTriage)
	}
	if got, _ := stageTrackerModifiers([]string{"bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false, "", "issue", stagelabels.Actionable, false); len(got) != 0 {
		t.Fatalf("modifiers = %v, want none", got)
	}
}

// An issue at 015-triage-needed lights the Actionable column with its own
// word, so the redundant needs-triage chip is dropped there (as the
// needs-decision chip is on 045). On a claimed issue the label is stale —
// 015 is not a gate and the stage word is the truth — so the chip is
// dropped too and the label is reported as stale_labels for the operator
// tooltip (hivecommons/hive#14586). The caption names the maintainer as
// the next mover (hivecommons/hive#13268).
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
	if issue21.Actor != "maintainer" || issue21.Caption != stageTrackerTriageNeededCaption {
		t.Fatalf("issue21 caption=%q actor=%q", issue21.Caption, issue21.Actor)
	}
	if !strings.Contains(issue21.Reason, "filed by the hive") {
		t.Fatalf("reason %q must carry the derivation; the caption no longer repeats it", issue21.Reason)
	}
	issue22 := trackerItem(t, resp, "issue", 22)
	if issue22.Stage != trackerNumberClaimed || hasMod(issue22, modNeedsTriage) {
		t.Fatalf("a claimed issue must not read as triage needed: %+v", issue22)
	}
	if !slices.Equal(issue22.StaleLabels, []string{modNeedsTriage}) {
		t.Fatalf("stale needs-triage must be reported for the operator tooltip: %+v", issue22.StaleLabels)
	}
	if len(issue21.StaleLabels) != 0 {
		t.Fatalf("015 wears needs-triage legitimately; stale_labels = %v", issue21.StaleLabels)
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

// The needs-triage chip is kept before the claim: at 010 and on the 045
// park it still tells the operator something, and nothing is reported
// stale. Past the claim (020, 030, 040, and an issue mirroring its covering
// PR's stage) the chip goes and stale_labels names it.
func TestStageTracker_NeedsTriageStaleOnlyPastClaim(t *testing.T) {
	now := time.Date(2026, 10, 27, 17, 40, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	cases := []struct {
		number int
		stage  stagelabels.Stage
		stale  bool
	}{
		{31, stagelabels.Actionable, false},
		{32, stagelabels.IssueNeedsDecision, false},
		{33, stagelabels.Claimed, true},
		{34, stagelabels.PRInProgress, true},
		{35, stagelabels.CoveredByPR, true},
		{36, stagelabels.ReviewPending, true},
	}
	var issues []any
	for _, c := range cases {
		store.Observe(stagelabels.Key("acme/console", c.number), c.stage, "test")
		issues = append(issues, github.Issue{Number: c.number, Title: "n", Author: "newcomer", AuthorIsHuman: true, Labels: []string{c.stage.Label(), "needs-triage"}})
	}
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{Name: "console", Full: "acme/console", ActionableIssues: issues}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)
	for _, c := range cases {
		it := trackerItem(t, resp, "issue", c.number)
		if it.Stage != c.stage.Number() {
			t.Fatalf("#%d stage = %d, want %d", c.number, it.Stage, c.stage.Number())
		}
		if hasMod(it, modNeedsTriage) == c.stale {
			t.Fatalf("#%d at %s: needs-triage modifier present=%v, want %v", c.number, c.stage, hasMod(it, modNeedsTriage), !c.stale)
		}
		if gotStale := slices.Contains(it.StaleLabels, modNeedsTriage); gotStale != c.stale {
			t.Fatalf("#%d at %s: stale_labels = %v, want stale=%v", c.number, c.stage, it.StaleLabels, c.stale)
		}
	}
}

// A reporter-trust hold the awaiting label has not reached yet
// (hivecommons/hive#15437: stage 0, bucket reporter_triage, labels null,
// modifiers []) carries the needs-triage modifier all the same, so the Open
// chevron reads "Triage needed" rather than "New" and Accept triage leads.
// The caption keeps the next-pass narration.
func TestStageTracker_ReporterTrustHoldReadsTriageNeeded(t *testing.T) {
	now := time.Date(2026, 10, 29, 18, 20, 0, 0, time.UTC)
	opened := now.Add(-28 * time.Minute)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Governor.NextKickAt = now.Add(4 * time.Minute).UTC().Format(time.RFC3339)
	status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive", NonActionableIssues: []any{
		github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 15437, Title: "ServiceMonitor component", Author: "onkar717", AuthorIsHuman: true,
			CreatedAt: opened, UpdatedAt: opened, Bucket: "reporter_triage", Reason: "Awaiting maintainer triage label"},
	}}}
	s.UpdateStatus(status)
	it := trackerItem(t, s.buildStageTracker(s.status, store, now), "issue", 15437)
	if it.Stage != 0 || it.StageSlug != StageTrackerNewSlug || it.New == nil || !it.New.TriagePending() {
		t.Fatalf("#15437 = stage %d slug %q new %+v, want 000-new with triage pending", it.Stage, it.StageSlug, it.New)
	}
	if !slices.Equal(it.Modifiers, []string{modNeedsTriage}) {
		t.Fatalf("#15437 modifiers = %v, want [%s] without the label", it.Modifiers, modNeedsTriage)
	}
	if it.Actor != StageTrackerMoverMaintainer || it.Caption != stageTrackerTriageNeededCaption || it.New == nil || it.New.NextPassInSeconds != 4*60 {
		t.Fatalf("#15437 caption %q actor %q new %+v, want %q with the next pass on new", it.Caption, it.Actor, it.New, stageTrackerTriageNeededCaption)
	}
}
