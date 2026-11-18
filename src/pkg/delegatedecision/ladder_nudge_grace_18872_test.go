package delegatedecision

import (
	"strings"
	"testing"
)

// nudgeGracePatterns are the four nudge-then-adopt pairs of
// hivecommons/hive#18872, as the ladder sees each head: the lane reason the
// nudge named and the head facts behind it. Every one is a clanker PR whose
// relay is not connected.
func nudgeGracePatterns() []struct {
	name string
	in   Input
} {
	base := Input{Repo: "hivecommons/hive", Kind: KindPR, Author: "contributor", AuthorTooling: AuthorToolingClanker, ContributorOnline: false}
	return []struct {
		name string
		in   Input
	}{
		// #18561: the review swarm asked for changes and a check is red.
		{"contributor-lane", with(base, func(i *Input) {
			i.Number, i.Reasons = 18561, []string{"contributor-lane:changes-requested"}
		})},
		// #18628: the branch is behind and Hive cannot update the fork; CI
		// on the stale head is red.
		{"fork-not-editable", with(base, func(i *Input) {
			i.Number, i.Reasons = 18628, []string{"fork-not-editable"}
		})},
		// #18514: a required check failed.
		{"check-failing", with(base, func(i *Input) {
			i.Number, i.Reasons = 18514, []string{"check-failing:unit"}
		})},
		// #18510: the head conflicts with the base; CI itself is green.
		{"conflicting", with(base, func(i *Input) {
			i.Number, i.Reasons, i.Conflicting, i.CIGreen = 18510, []string{"conflicting"}, true, true
		})},
	}
}

// Invariant 1 (hivecommons/hive#18872): a head the ladder supersedes at
// once is one the nudge sender must not nudge. SupersedesImmediately is
// that answer and agrees with the full policy.
func TestSupersedesImmediatelyMatchesLadder18872(t *testing.T) {
	for _, p := range nudgeGracePatterns() {
		t.Run(p.name, func(t *testing.T) {
			if !SupersedesImmediately(p.in) {
				t.Fatalf("offline relay, %s head: SupersedesImmediately = false, want the ladder's immediate supersede", p.name)
			}
			if d := Decide(p.in); d.Verdict != VerdictAdoptSupersede || d.Rule != RuleContributorOffline {
				t.Fatalf("Decide = %s/%s, want adopt-supersede/%s", d.Verdict, d.Rule, RuleContributorOffline)
			}
			// A connected relay gets the PR back with the fix list: that
			// head is nudged, and the ladder does not supersede it.
			online := with(p.in, func(i *Input) { i.ContributorOnline = true })
			if SupersedesImmediately(online) {
				t.Fatalf("online relay, %s head: SupersedesImmediately = true", p.name)
			}
			// A mergeable, green head is the merge queue's, never adopted.
			green := with(p.in, func(i *Input) { i.Conflicting, i.CIGreen = false, true })
			if SupersedesImmediately(green) {
				t.Fatalf("green %s head: SupersedesImmediately = true", p.name)
			}
		})
	}
}

// Invariant 2 (hivecommons/hive#18872): a head the hive did nudge is not
// superseded before the grace. While NudgeGracePending holds the ladder
// recommends waiting (no adopt action, park kept); once the grace has run
// out the immediate supersede applies again.
func TestNudgedHeadKeepsGrace18872(t *testing.T) {
	for _, p := range nudgeGracePatterns() {
		t.Run(p.name, func(t *testing.T) {
			nudged := with(p.in, func(i *Input) { i.NudgeGracePending = true })
			if SupersedesImmediately(nudged) {
				t.Fatalf("nudged %s head inside the grace: SupersedesImmediately = true", p.name)
			}
			d := Decide(nudged)
			if d.Verdict == VerdictAdoptSupersede || d.Rule != RuleNudgeGrace || !d.KeepsPark {
				t.Fatalf("nudged %s head inside the grace: %s/%s keeps_park=%v, want recommend-only/%s", p.name, d.Verdict, d.Rule, d.KeepsPark, RuleNudgeGrace)
			}
			for _, a := range d.Actions {
				if a.Kind == ActionAdoptSupersede {
					t.Fatalf("nudged %s head inside the grace carries an adopt action: %+v", p.name, d.Actions)
				}
			}
			if d.Ladder == nil || d.Ladder.Decision != RuleNudgeGrace || !strings.Contains(strings.Join(d.Ladder.RuledOut, "\n"), "nudged on this head") {
				t.Fatalf("ladder = %+v", d.Ladder)
			}
			if _, ok := Explain(RuleNudgeGrace); !ok {
				t.Fatalf("%s has no glossary entry", RuleNudgeGrace)
			}
			// Grace over: the immediate supersede is back.
			if !SupersedesImmediately(p.in) {
				t.Fatalf("%s head after the grace: SupersedesImmediately = false", p.name)
			}
			// A stalled adoption is not a fresh nudge: it is superseded.
			stalled := with(nudged, func(i *Input) { i.AdoptionStalled = true })
			if d := Decide(stalled); d.Verdict != VerdictAdoptSupersede {
				t.Fatalf("stalled adoption on a nudged %s head: %s/%s, want adopt-supersede", p.name, d.Verdict, d.Rule)
			}
		})
	}
}
