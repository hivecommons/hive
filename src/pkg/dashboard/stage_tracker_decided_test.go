package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15126: a recorded "Let hive decide" projects the row
// onto one coherent post-decision state.
func TestStageTrackerDecidedProjection(t *testing.T) {
	now := time.Date(2026, 10, 21, 17, 55, 0, 0, time.UTC)
	decidedAt := now.Add(-4 * time.Minute)
	sweep := automerge.SweepState{NextFloorAt: now.Add(3 * time.Minute)}
	s := &Server{}
	s.sweepState = func() automerge.SweepState { return sweep }
	act := func(verdict, state string) *StageTrackerOperatorAction {
		return &StageTrackerOperatorAction{Action: operatorActionLetHiveDecide, Actor: "clubanderson", At: decidedAt,
			Outcome: operatorActionOutcomeApplied, State: state, Verdict: verdict, Rule: "contributor-offline", StageAt: stagelabels.ReadyForHumanMerge.Number()}
	}
	pr090 := func(n int, a *StageTrackerOperatorAction) StageTrackerItem {
		return StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: n, Author: "Karibhou", Stage: stagelabels.ReadyForHumanMerge.Number(),
			StageSlug: stagelabels.ReadyForHumanMerge.Slug(), Modifiers: []string{modNeedsHuman}, Caption: "Ready for human merge · untrusted author",
			HumanMergeReason: "untrusted-author", HumanMergeReasonText: "untrusted author", ManualMerge: true, LastOperatorAction: a}
	}

	t.Run("pending adopt-supersede leaves 090 for the merge gate", func(t *testing.T) {
		items := []StageTrackerItem{
			pr090(15029, act(string(delegatedecision.VerdictAdoptSupersede), operatorActionStateWaiting)),
			{Kind: "issue", Repo: "hivecommons/hive", Number: 15023, Stage: stagelabels.ReadyForHumanMerge.Number(), Mirrored: true, LinkedPR: 15029,
				Modifiers: []string{modNeedsHuman}, Reason: coverReason(15029, stagelabels.ReadyForHumanMerge), HumanMergeReason: "untrusted-author"},
		}
		s.applyDecidedProjection(items, now)
		pr, issue := items[0], items[1]
		d := pr.Decided
		if d == nil || d.Effect != decidedEffectPending || d.Verdict != "adopt-supersede" {
			t.Fatalf("decided = %+v", d)
		}
		if pr.Stage != stagelabels.AwaitingMergeAdmission.Number() || d.StageBefore != stagelabels.ReadyForHumanMerge.Number() {
			t.Fatalf("stage %d (before %d), want 080 projected from 090", pr.Stage, d.StageBefore)
		}
		if pr.HumanMergeReason != "" || pr.ManualMerge || strings.Contains(pr.Caption, "untrusted") || contains(pr.Modifiers, modNeedsHuman) {
			t.Fatalf("090 needs-human state survived the decision: %+v", pr)
		}
		if pr.Caption != "Adopted · hive rebases & merges next pass" || d.Acting != "Adopting · opening hive copy…" {
			t.Fatalf("caption %q acting %q", pr.Caption, d.Acting)
		}
		if d.NextAt == nil || !d.NextAt.Equal(sweep.NextFloorAt) {
			t.Fatalf("next_at = %v, want the sweep floor", d.NextAt)
		}
		for _, want := range []string{"You chose Let hive decide · hive decided: adopted (supersede)", "17:51 UTC", "hive chose adopt-supersede", "next: adoption pass in 3m"} {
			if !strings.Contains(d.Title, want) {
				t.Errorf("title %q lacks %q", d.Title, want)
			}
		}
		if strings.Contains(d.Title, "Approv") || strings.Contains(d.Acting, "Approv") || strings.Contains(pr.Caption, "Approv") {
			t.Fatalf("an adopt decision must never read as approving: %q / %q / %q", d.Title, d.Acting, pr.Caption)
		}
		if pr.MergeGate == nil || pr.Actor != StageActorHive {
			t.Fatalf("merge gate %+v actor %q", pr.MergeGate, pr.Actor)
		}
		if issue.Stage != stagelabels.AwaitingMergeAdmission.Number() || issue.Decided == nil || issue.HumanMergeReason != "" || issue.Reason != coverReason(15029, stagelabels.AwaitingMergeAdmission) {
			t.Fatalf("issue riding the PR did not follow: %+v", issue)
		}
	})

	t.Run("landed adoption pairs original with the hive copy", func(t *testing.T) {
		orig := pr090(15029, act(string(delegatedecision.VerdictAdoptSupersede), operatorActionStateWaiting))
		orig.Stage, orig.AdoptedBy = stagelabels.AdoptedByHive.Number(), 15123
		copy := StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 15123, Author: "hivecommons-hive[bot]", Stage: stagelabels.ReviewPending.Number(), Caption: "CI / Review"}
		issue := StageTrackerItem{Kind: "issue", Repo: "hivecommons/hive", Number: 15023, Stage: stagelabels.ReadyForHumanMerge.Number(), Mirrored: true, LinkedPR: 15029,
			Modifiers: []string{modNeedsHuman}, Caption: "Ready for human merge · untrusted author"}
		items := []StageTrackerItem{orig, copy, issue}
		s.applyDecidedProjection(items, now)
		o, c, is := items[0], items[1], items[2]
		if o.Decided == nil || o.Decided.Effect != decidedEffectLanded || o.Decided.SupersededBy != 15123 || o.Stage != stagelabels.AdoptedByHive.Number() {
			t.Fatalf("original: %+v", o.Decided)
		}
		if o.Decided.Text != "superseded · hive adopted → #15123" {
			t.Fatalf("original text %q", o.Decided.Text)
		}
		if c.AdoptedFrom == nil || c.AdoptedFrom.Number != 15029 || c.AdoptedFrom.DecidedBy != "clubanderson" || c.AdoptedFrom.Text != "↳ adopted from #15029 · you let hive decide 17:51 UTC" {
			t.Fatalf("copy adopted_from: %+v", c.AdoptedFrom)
		}
		if is.LinkedPR != 15123 || is.Stage != stagelabels.ReviewPending.Number() || is.Caption != "CI / Review" || contains(is.Modifiers, modNeedsHuman) {
			t.Fatalf("issue must follow the hive copy: %+v", is)
		}
	})

	t.Run("closed original renders as one closed line, never a bare number", func(t *testing.T) {
		store := stagelabels.NewStore("", nil)
		key := stagelabels.Key("hivecommons/hive", 15029)
		store.Observe(key, stagelabels.AdoptedByHive, "adopted")
		store.MarkAdopted(key, "feedfacefeedfacefeedfacefeedfacefeedface", 15123, decidedAt)
		s2 := &Server{stageStore: store}
		s2.sweepState = func() automerge.SweepState { return sweep }
		copy := StageTrackerItem{Kind: "pr", Repo: "hivecommons/hive", Number: 15123, Title: "fix: decided state", Author: "hivecommons-hive[bot]", Stage: stagelabels.ReviewPending.Number(), Caption: "CI / Review"}
		// The issue still names the closed original as its covering PR; the
		// open-PR projection no longer carries it.
		issue := StageTrackerItem{Kind: "issue", Repo: "hivecommons/hive", Number: 15023, Stage: stagelabels.ReadyForHumanMerge.Number(), Mirrored: true, LinkedPR: 15029,
			Modifiers: []string{modNeedsHuman}, Caption: "Ready for human merge · untrusted author"}
		items := []StageTrackerItem{copy, issue}
		s2.applyDecidedProjection(items, now)
		c, is := items[0], items[1]
		if is.LinkedPR != 15123 || is.Stage != stagelabels.ReviewPending.Number() || contains(is.Modifiers, modNeedsHuman) {
			t.Fatalf("issue must follow the copy: %+v", is)
		}
		if len(is.LinkedPRs) != 1 {
			t.Fatalf("linked_prs = %+v", is.LinkedPRs)
		}
		orig := is.LinkedPRs[0]
		if orig.Number != 15029 || orig.State != "closed" || orig.SupersededBy != 15123 || orig.Title != "fix: decided state" || orig.Caption != "superseded · closed · hive adopted → #15123" {
			t.Fatalf("closed original = %+v", orig)
		}
		if c.AdoptedFrom == nil || c.AdoptedFrom.Number != 15029 {
			t.Fatalf("copy adopted_from = %+v", c.AdoptedFrom)
		}
	})

	t.Run("timed out falls back to the real stage with options", func(t *testing.T) {
		a := act(string(delegatedecision.VerdictAdoptSupersede), operatorActionStateWaiting)
		a.At = now.Add(-delegatedecision.DefaultEffectTimeout - time.Minute)
		items := []StageTrackerItem{pr090(15029, a)}
		s.applyDecidedProjection(items, now)
		pr := items[0]
		if pr.Decided == nil || pr.Decided.Effect != decidedEffectTimedOut || pr.Stage != stagelabels.ReadyForHumanMerge.Number() || pr.HumanMergeReason == "" {
			t.Fatalf("timed-out: stage %d decided %+v", pr.Stage, pr.Decided)
		}
		if !strings.Contains(pr.Decided.Title, "not in effect after 30m") {
			t.Fatalf("title %q", pr.Decided.Title)
		}
	})

	t.Run("approve reads approving; other verdicts never do", func(t *testing.T) {
		for verdict, verb := range decidedVerbs {
			isApprove := verdict == string(delegatedecision.VerdictApprove)
			if strings.Contains(verb.Acting, "Approving") != isApprove {
				t.Errorf("%s acting %q", verdict, verb.Acting)
			}
			if !isApprove && (strings.Contains(verb.Text, "Approving") || strings.Contains(verb.Plain, "approving")) {
				t.Errorf("%s names approving: %+v", verdict, verb)
			}
		}
		items := []StageTrackerItem{pr090(1, act(string(delegatedecision.VerdictApprove), operatorActionStateWaiting))}
		s.applyDecidedProjection(items, now)
		if items[0].Decided.Acting != "Approving…" || !strings.Contains(items[0].Decided.Title, "approved") {
			t.Fatalf("approve: %+v", items[0].Decided)
		}
	})

	t.Run("recommend-only keeps the park", func(t *testing.T) {
		items := []StageTrackerItem{pr090(1, act(string(delegatedecision.VerdictRecommendOnly), operatorActionStateWaiting))}
		s.applyDecidedProjection(items, now)
		pr := items[0]
		if pr.Decided == nil || pr.Decided.Effect != decidedEffectNone || pr.Stage != stagelabels.ReadyForHumanMerge.Number() || pr.Decided.Acting != "" {
			t.Fatalf("%+v", pr.Decided)
		}
	})

	// hivecommons/hive#15096 N13: hive decided direction (fix then merge)
	// on #15020 while the stale `needs-human` label kept the row in 085
	// re-asking the question. The decision wins: the row sits at 070
	// Fixing with the review it is addressing, no decision_needed.
	t.Run("direction decided while still parked in 085 projects to 070 Fixing", func(t *testing.T) {
		a := act(string(delegatedecision.VerdictDirection), operatorActionStateWaiting)
		a.Rule, a.StageAt = "fix-then-merge", stagelabels.NeedsDecision.Number()
		items := []StageTrackerItem{{Kind: "pr", Repo: "hivecommons/hive", Number: 15020, Stage: stagelabels.NeedsDecision.Number(),
			StageSlug: stagelabels.NeedsDecision.Slug(), Modifiers: []string{modNeedsHuman}, Caption: "Needs human decision",
			Reason: "contributor-lane:changes-requested", LastOperatorAction: a,
			DecisionNeeded: &StageTrackerDecision{Question: "@hanthor requested changes on this head", AskedBy: decisionAskedByReviewer}}}
		s.applyDecidedProjection(items, now)
		pr := items[0]
		if pr.Decided == nil || pr.Decided.Effect != decidedEffectPending {
			t.Fatalf("decided = %+v", pr.Decided)
		}
		if pr.Stage != stagelabels.Fixing.Number() || pr.Decided.StageBefore != stagelabels.NeedsDecision.Number() || pr.Decided.Stage != stagelabels.Fixing.Number() {
			t.Fatalf("stage %d (before %d, decided.stage %d), want 070 projected from 085", pr.Stage, pr.Decided.StageBefore, pr.Decided.Stage)
		}
		if pr.DecisionNeeded != nil || contains(pr.Modifiers, modNeedsHuman) || pr.MergeGate != nil {
			t.Fatalf("stale 085 ask survived the decision: %+v", pr)
		}
		if pr.Caption != "Fixing · hive is addressing @hanthor's review" {
			t.Fatalf("caption %q", pr.Caption)
		}
		if !strings.HasPrefix(pr.Decided.Title, "You chose Let hive decide · hive decided: direction given (") {
			t.Fatalf("title %q must lead with the choice, not the stale ask", pr.Decided.Title)
		}
	})

	t.Run("direction decided on a 070 row keeps its stage", func(t *testing.T) {
		a := act(string(delegatedecision.VerdictDirection), operatorActionStateWaiting)
		a.StageAt = stagelabels.Fixing.Number()
		items := []StageTrackerItem{{Kind: "pr", Repo: "hivecommons/hive", Number: 15020, Stage: stagelabels.Fixing.Number(), LastOperatorAction: a}}
		s.applyDecidedProjection(items, now)
		if items[0].Stage != stagelabels.Fixing.Number() || items[0].Decided == nil || items[0].Decided.Stage != 0 {
			t.Fatalf("%+v", items[0])
		}
	})

	t.Run("no decision, no projection", func(t *testing.T) {
		items := []StageTrackerItem{pr090(1, &StageTrackerOperatorAction{Action: operatorActionHold, Outcome: operatorActionOutcomeApplied, At: decidedAt})}
		s.applyDecidedProjection(items, now)
		if items[0].Decided != nil || items[0].Stage != stagelabels.ReadyForHumanMerge.Number() {
			t.Fatalf("%+v", items[0])
		}
	})
}

// The glossary never calls a contributor's agent their "hive": it is their
// clanker (operator correction on #14947's popover).
func TestDecisionGlossaryNamesClankerNotHive(t *testing.T) {
	for _, bad := range []string{"author's hive", "contributor's hive", "their hive"} {
		if strings.Contains(delegatedecision.GlossaryJSON(), bad) {
			t.Fatalf("glossary says %q; a contributor's agent is their clanker", bad)
		}
	}
}
