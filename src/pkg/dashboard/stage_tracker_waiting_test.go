package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The waiting_on_human block names every item parked on the operator /
// maintainers so the SPA badges, pill and toast can never stay silent
// while a dozen PRs sit at 085 (hivecommons/hive#15638).
func TestStageTrackerWaitingOnHuman_KindsAndAttribution(t *testing.T) {
	asked := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	older := asked.Add(-6 * time.Hour)
	items := []StageTrackerItem{
		// 085 with a recorded decision question: needs-decision, since = asked.
		{Repo: "o/r", Number: 15312, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), DecisionNeeded: &StageTrackerDecision{Since: &asked}},
		// 045 issue: needs-decision too.
		{Repo: "o/r", Number: 20, Kind: "issue", Stage: stagelabels.IssueNeedsDecision.Number(), StageSince: &older},
		// 090: a trusted human must press merge.
		{Repo: "o/r", Number: 15433, Kind: "pr", Stage: stagelabels.ReadyForHumanMerge.Number()},
		// unauthorized modifier on an ordinary stage: approve-needed.
		{Repo: "o/r", Number: 30, Kind: "pr", Stage: 60, Modifiers: []string{modUnauthorized}},
		// 072 with the contributor still inside their grace: not the
		// operator's wait.
		{Repo: "o/r", Number: 40, Kind: "pr", Stage: stagelabels.ReturnedToContributor.Number(), Author: "karibhou", Presence: &StageTrackerPresence{Login: "karibhou", Next: presenceNextWait}},
		// 071 with the grace elapsed: the operator is the fallback, the
		// wait is attributed to the contributor.
		{Repo: "o/r", Number: 41, Kind: "pr", Stage: stagelabels.NeedsAuthor.Number(), Author: "karibhou", Takeover: &StageTrackerTakeover{GraceElapsed: true}},
		// 015 triage-needed: other-human-wait.
		{Repo: "o/r", Number: 50, Kind: "issue", Stage: stagelabels.TriageNeeded.Number()},
		// hold, advisory and decided rows never count.
		{Repo: "o/r", Number: 60, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), Modifiers: []string{modHold}},
		{Repo: "o/r", Number: 61, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), Advisory: &StageTrackerAdvisory{}},
		{Repo: "o/r", Number: 62, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), Decided: &StageTrackerDecided{}},
		// a nested PR is counted once, under its issue.
		{Repo: "o/r", Number: 63, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), NestedUnder: 20},
		// an ordinary in-flight PR waits on nobody.
		{Repo: "o/r", Number: 70, Kind: "pr", Stage: 60},
	}
	got := stageTrackerWaitingOnHuman(items)
	if got.Count != 6 || len(got.Items) != 6 {
		t.Fatalf("count = %d (%d items), want 6: %+v", got.Count, len(got.Items), got.Items)
	}
	want := map[int][2]string{
		15312: {waitKindNeedsDecision, waitWhoOperator},
		20:    {waitKindNeedsDecision, waitWhoOperator},
		15433: {waitKindReadyForHumanMerge, waitWhoOperator},
		30:    {waitKindApproveNeeded, waitWhoOperator},
		41:    {waitKindReturnedOffline, "karibhou"},
		50:    {waitKindOther, waitWhoOperator},
	}
	for _, w := range got.Items {
		exp, ok := want[w.Number]
		if !ok {
			t.Errorf("unexpected item #%d (%s)", w.Number, w.Kind)
			continue
		}
		if w.Kind != exp[0] || w.Who != exp[1] {
			t.Errorf("#%d: kind/who = %s/%s, want %s/%s", w.Number, w.Kind, w.Who, exp[0], exp[1])
		}
		if w.Number == 15312 && (w.Type != "pr" || w.Since == nil || !w.Since.Equal(asked)) {
			t.Errorf("#15312: type/since = %s/%v, want pr/%v", w.Type, w.Since, asked)
		}
	}
	// Longest wait first; items with no Since sink to the end.
	if got.Items[0].Number != 20 || got.Items[1].Number != 15312 {
		t.Errorf("order = %d, %d; want 20 then 15312", got.Items[0].Number, got.Items[1].Number)
	}
	// Per-item flags mirror the block so the SPA filter shares one truth.
	for _, it := range items {
		_, flagged := want[it.Number]
		if it.WaitingOnYou != flagged {
			t.Errorf("#%d: waiting_on_you = %v, want %v", it.Number, it.WaitingOnYou, flagged)
		}
		if flagged && it.WaitKind != want[it.Number][0] {
			t.Errorf("#%d: wait_kind = %q, want %q", it.Number, it.WaitKind, want[it.Number][0])
		}
	}
}

func TestStageTrackerWaitingOnHuman_ZeroWhenNone(t *testing.T) {
	got := stageTrackerWaitingOnHuman([]StageTrackerItem{{Repo: "o/r", Number: 1, Kind: "pr", Stage: 60}})
	if got.Count != 0 || got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("got %+v, want count 0 with an empty (non-nil) list", got)
	}
	if stageTrackerWaitingOnHuman(nil).Count != 0 {
		t.Fatal("nil items must count zero")
	}
}

// Adoption.Due and Presence.Next=adopt are the other two signals that the
// contributor's grace ran out.
func TestStageTrackerWaitingOnHuman_ReturnedOfflineSignals(t *testing.T) {
	stage := stagelabels.ReturnedToContributor.Number()
	for name, it := range map[string]StageTrackerItem{
		"adoption-due":   {Number: 1, Kind: "pr", Stage: stage, Adoption: &StageTrackerAdoption{Author: "zed", Due: true}},
		"presence-adopt": {Number: 2, Kind: "pr", Stage: stage, Author: "zed", Presence: &StageTrackerPresence{Login: "zed", Next: presenceNextAdopt}},
	} {
		kind, who := stageTrackerWaitKind(it)
		if kind != waitKindReturnedOffline || who != "zed" {
			t.Errorf("%s: kind/who = %s/%s, want %s/zed", name, kind, who, waitKindReturnedOffline)
		}
	}
}

// A standing meta issue waits on nobody, whatever it wears: the four
// Renovate Dependency Dashboards on projectbluefin (common#245,
// gutenprint-printer-app#45, hplip-printer-app#42, ps-printer-app#50)
// carry needs-human from a triage bot and were listed as approve-needed.
// A sidelined issue in any other bucket still counts.
func TestStageTrackerWaitingOnHuman_SkipsStandingMetaIssues(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "projectbluefin/common", Number: 245, Kind: "issue", Author: "renovate[bot]", Bucket: repoWorkBucketDependencyDashboard, Modifiers: []string{modNeedsHuman}},
		{Repo: "projectbluefin/hplip-printer-app", Number: 42, Kind: "issue", Author: "mergeraptor[bot]", Bucket: repoWorkBucketDependencyDashboard, Stage: stagelabels.IssueNeedsDecision.Number(), Modifiers: []string{modNeedsHuman}},
		{Repo: "o/r", Number: 53, Kind: "issue", Bucket: repoWorkBucketHiveAdvisory, Modifiers: []string{modNeedsDecision}},
		// control: a needs-human issue in the filtered bucket is a real ask.
		{Repo: "o/r", Number: 54, Kind: "issue", Bucket: repoWorkBucketFiltered, Modifiers: []string{modNeedsHuman}},
		// the bucket only marks issues; a PR is classified as before.
		{Repo: "o/r", Number: 60, Kind: "pr", Bucket: repoWorkBucketDependencyDashboard, Stage: 60, Modifiers: []string{modNeedsHuman}},
	}
	got := stageTrackerWaitingOnHuman(items)
	if got.Count != 2 || len(got.Items) != 2 {
		t.Fatalf("count = %d (%+v), want 2 (#54 and PR #60)", got.Count, got.Items)
	}
	for _, it := range items {
		standing := it.Kind == "issue" && it.Number != 54
		if it.WaitingOnYou == standing {
			t.Errorf("%s#%d (bucket %s): waiting_on_you = %v, want %v", it.Repo, it.Number, it.Bucket, it.WaitingOnYou, !standing)
		}
		if !standing && it.WaitKind != waitKindApproveNeeded {
			t.Errorf("%s#%d: wait_kind = %q, want %q", it.Repo, it.Number, it.WaitKind, waitKindApproveNeeded)
		}
	}
}
