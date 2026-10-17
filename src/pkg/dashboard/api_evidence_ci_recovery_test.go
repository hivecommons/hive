package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestBuildEvidenceCIRecovery pins the CI gate panel: gate integrity over
// merged PRs, convergence passes over agent PRs, and the recovery read: the claim
// snapshot is the first live record that captured CI; red-at-claim PRs are
// bucketed by what happened next (merged = recovered, green but open,
// abandoned, still pending, outcome unknown); medians come only from PRs
// the GitHub trail has reached; nothing unknown counts as a failure.
func TestBuildEvidenceCIRecovery(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-48 * time.Hour)
	red := []evidence.Check{{Name: "build", Conclusion: "failure"}}
	green := []evidence.Check{{Name: "build", Conclusion: "success"}}
	live := func(n int, head string, at time.Time, checks []evidence.Check) *evidence.Bundle {
		return &evidence.Bundle{Repo: "o/r", Number: n, HeadSHA: head, Author: evidence.Author{Login: "bot", Kind: evidence.AuthorAgent}, CreatedAt: at, UpdatedAt: at, CI: evidence.CI{Checks: checks}}
	}
	// 1: red at claim, merged (trail: push → green, 2h) → recovered
	b1 := live(1, "a1", t0, red)
	b1m := live(1, "a2", t0.Add(3*time.Hour), green)
	b1m.Merge = &evidence.MergeEvent{At: t0.Add(3 * time.Hour)}
	// 2: red at claim, trail says green on the same head, merged → recovered, 0 pushes
	b2 := live(2, "b1", t0, red)
	// 3: red at claim, closed without merge (backfill) → abandoned
	b3 := live(3, "c1", t0, red)
	b3c := &evidence.Bundle{Repo: "o/r", Number: 3, HeadSHA: "c1", Author: evidence.Author{Login: "bot", Kind: evidence.AuthorAgent}, CreatedAt: t0, UpdatedAt: now, Backfilled: true, Backfill: &evidence.Backfill{State: evidence.BackfillStateClosed, ClosedAt: now}}
	// 4: red at claim, no trail, no outcome → outcome unknown, not enriched
	b4 := live(4, "d1", t0, red)
	// 5: first record had no CI, second was green → green at claim
	b5a := live(5, "e1", t0, nil)
	b5b := live(5, "e2", t0.Add(time.Hour), green)
	// 6: human PR is not a claim
	b6 := live(6, "f1", t0, red)
	b6.Author.Kind = evidence.AuthorHuman
	// 7: red at claim, trail: still red, PR open → pending
	b7 := live(7, "g1", t0, red)
	// 8: red at claim, later live record green, not merged, no trail → green open
	b8 := live(8, "h1", t0, red)
	b8b := live(8, "h2", t0.Add(time.Hour), green)
	// 9: human PR whose review-time snapshot was red on the merged head and
	// no GitHub trail yet → suspected, not a breach (a re-run may have gone green)
	b9 := live(9, "i1", t0, red)
	b9.Author.Kind = evidence.AuthorHuman
	b9.Merge = &evidence.MergeEvent{At: t0.Add(time.Hour)}
	// 10: human PR merged with no bundle snapshot; the GitHub trail's last
	// head is green 3h before the merge → green at merge, gate cost 3h
	b10 := live(10, "j1", t0, nil)
	b10.Author.Kind = evidence.AuthorHuman
	b10.Merge = &evidence.MergeEvent{At: t0.Add(4 * time.Hour)}
	// 11: agent PR red at claim, merged on a new head nobody snapshotted →
	// gate unknown, convergence unknown (never counted as a breach)
	b11 := live(11, "k1", t0, red)
	b11m := live(11, "k2", t0.Add(2*time.Hour), nil)
	b11m.Merge = &evidence.MergeEvent{At: t0.Add(2 * time.Hour)}
	// 12: human PR merged red, confirmed by GitHub's trail → breach
	b12 := live(12, "l1", t0, red)
	b12.Author.Kind = evidence.AuthorHuman
	b12.Merge = &evidence.MergeEvent{At: t0.Add(time.Hour)}
	// 13: human PR merged green per bundle (build only), no trail → green
	// from bundles; under a policy requiring lint it is suspected missing_required
	b13 := live(13, "m1", t0, green)
	b13.Author.Kind = evidence.AuthorHuman
	b13.Merge = &evidence.MergeEvent{At: t0.Add(time.Hour)}
	// 14: human PR, bundle red (an optional docs check), but GitHub's trail
	// judged the merged head on the required set: green with a non-required
	// red disclosed → green, not a breach, not suspected
	b14 := live(14, "n1", t0, red)
	b14.Author.Kind = evidence.AuthorHuman
	b14.Merge = &evidence.MergeEvent{At: t0.Add(time.Hour)}
	// 15: human PR whose merged head never had a required check report,
	// per GitHub → suspected missing_required (a path-filtered skip the gate
	// accepts cannot be told from a truly missing check here), not a breach
	b15 := live(15, "p1", t0, nil)
	b15.Author.Kind = evidence.AuthorHuman
	b15.Merge = &evidence.MergeEvent{At: t0.Add(time.Hour)}
	bundles := []*evidence.Bundle{b1, b1m, b2, b3, b3c, b4, b5a, b5b, b6, b7, b8, b8b, b9, b10, b11, b11m, b12, b13, b14, b15}
	prs := evidence.FoldPRs(bundles, "", "")
	claims := evidenceCIClaims(bundles, prs)
	merges := evidenceGateMerges(bundles, prs)
	if c := claims["o/r#5"]; c == nil || c.state != evidenceCIGreen || c.claimHead != "e2" {
		t.Fatalf("claim 5 = %+v (first CAPTURED snapshot is the claim)", c)
	}
	if _, ok := claims["o/r#6"]; ok {
		t.Fatal("human PR must not be a claim")
	}
	rollup := CIRecoveryRollup{PRs: map[string]CIRecoveryTrail{
		"o/r#1":  {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(3 * time.Hour), RequiredKnown: true, Evaluated: 2, PassesToGreen: 2, Heads: []ghpkg.PRCIHead{{SHA: "a1", At: t0, State: "red"}, {SHA: "a2", At: t0.Add(2 * time.Hour), State: "green"}}}},
		"o/r#2":  {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), RequiredKnown: true, Evaluated: 1, PassesToGreen: 1, Heads: []ghpkg.PRCIHead{{SHA: "b1", At: t0, State: "green"}}}},
		"o/r#7":  {Trail: ghpkg.PRCITrail{State: "open", Evaluated: 1, Heads: []ghpkg.PRCIHead{{SHA: "g1", At: t0, State: "red"}}}},
		"o/r#10": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(4 * time.Hour), Heads: []ghpkg.PRCIHead{{SHA: "j1", At: t0, State: "red"}, {SHA: "j2", At: t0.Add(time.Hour), State: "green"}}}},
		"o/r#12": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), MergedBy: "admin", RequiredKnown: true, Heads: []ghpkg.PRCIHead{{SHA: "l1", At: t0, State: "red", RequiredRed: []string{"test"}}}}},
		"o/r#14": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), RequiredKnown: true, Heads: []ghpkg.PRCIHead{{SHA: "n1", At: t0, State: "green", NonRequiredRed: []string{"docs-links"}}}}},
		"o/r#15": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), RequiredKnown: true, Heads: []ghpkg.PRCIHead{{SHA: "p1", At: t0, State: "pending", MissingRequired: []string{"coverage"}}}}},
	}}
	win := evidenceWindow{Hours: 168, From: now.Add(-168 * time.Hour), To: now}
	got := buildEvidenceCIRecovery(claims, merges, rollup, win, "", EvidenceCIPolicy{SelfAuthoredAutoMerge: true})
	if got.N != 8 || got.AtClaim["red"] != 7 || got.AtClaim["green"] != 1 || got.AtClaim["none"] != 0 {
		t.Errorf("n/at_claim = %d %+v", got.N, got.AtClaim)
	}
	r := got.Red
	if r.N != 7 || r.RecoveredMerged != 3 || r.GreenOpen != 1 || r.Abandoned != 1 || r.Pending != 1 || r.OutcomeUnknown != 1 {
		t.Errorf("red = %+v", r)
	}
	if r.Enriched != 3 || r.NotEnriched != 4 || r.SameHeadRecovered != 1 {
		t.Errorf("enrichment = %+v", r)
	}
	// Grade A, gate integrity: every merged PR (any author) by merge-time
	// check state as the merge gate judges it (required checks only).
	// Unknown and suspected are outside the denominator; only a required
	// check GitHub reports red under a resolved required set is a breach
	// (#12); a red NON-required check (#14) is green and disclosed; a
	// required check with no run (#15) and a bundle-only red (#9) are suspected.
	g := got.Gate
	if g.MergedN != 8 || g.WithChecks != 5 || g.Green != 4 || g.GreenFromBundles != 1 || g.Breaches != 1 || g.Suspected != 2 || g.Pending != 0 || g.Unknown != 1 || g.NonRequiredRed != 1 {
		t.Errorf("gate = %+v", g)
	}
	// #10's trail carries no RequiredKnown → judged on every check, disclosed.
	if g.RequiredUnknown != 1 {
		t.Errorf("RequiredUnknown = %d, want 1 (PR 10's trail)", g.RequiredUnknown)
	}
	if len(g.BreachPRs) != 1 || g.BreachPRs[0].Number != 12 || g.BreachPRs[0].State != "red" || g.BreachPRs[0].Red[0] != "test" || g.BreachPRs[0].Author != "human" || g.BreachPRs[0].Source != "github" || g.BreachPRs[0].MergedBy != "admin" {
		t.Errorf("breach list = %+v", g.BreachPRs)
	}
	if len(g.SuspectedPRs) != 2 || g.SuspectedPRs[0].Number != 15 || g.SuspectedPRs[0].State != "missing_required" || g.SuspectedPRs[0].Missing[0] != "coverage" || g.SuspectedPRs[0].Source != "github" {
		t.Errorf("suspected missing-required = %+v", g.SuspectedPRs)
	}
	if g.SuspectedPRs[1].Number != 9 || g.SuspectedPRs[1].Source != "bundle" {
		t.Errorf("suspected list = %+v", g.SuspectedPRs)
	}
	if g.GreenToMergeN != 3 || g.MedianGreenToMergeHours == nil || *g.MedianGreenToMergeHours != 1 {
		t.Errorf("gate cost = %d %v, want median 1h over 3h (#10 trail), 1h (#13 bundle), 1h (#14 trail)", g.GreenToMergeN, g.MedianGreenToMergeHours)
	}
	// A required check the bundle snapshot never saw green is suspected
	// missing_required (bundle-sourced, so unconfirmed); a trail-sourced
	// state has no check names, so it is not judged.
	strict := buildEvidenceCIRecovery(claims, merges, rollup, win, "", EvidenceCIPolicy{RequiredChecks: []string{"build", "lint"}})
	if strict.Gate.Breaches != 1 || strict.Gate.Green != 3 || strict.Gate.Suspected != 3 || len(strict.Gate.SuspectedPRs) != 3 || strict.Gate.SuspectedPRs[0].Number != 13 || strict.Gate.SuspectedPRs[0].State != "missing_required" || strict.Gate.SuspectedPRs[0].Missing[0] != "lint" {
		t.Errorf("strict gate = %+v", strict.Gate)
	}
	// Grade B, convergence: passes until the first green head, per agent PR;
	// in-flight and unknown PRs are counts, not failures; "never" is closed
	// unmerged without a green.
	c := got.Convergence
	if c.N != 8 || c.Settled != 5 || c.InFlight != 2 || c.Unknown != 1 {
		t.Errorf("convergence = %+v", c)
	}
	if c.Passes["1"] != 2 || c.Passes["2"] != 2 || c.Passes["3"] != 0 || c.Passes["4+"] != 0 || c.Passes["never"] != 1 {
		t.Errorf("passes = %v", c.Passes)
	}
	if c.FromGitHub != 2 || c.FromBundles != 3 {
		t.Errorf("sources = github %d bundles %d", c.FromGitHub, c.FromBundles)
	}
	if c.MedianPasses == nil || *c.MedianPasses != 1.5 {
		t.Errorf("median passes = %v, want 1.5 over 1,1,2,2", c.MedianPasses)
	}
	if len(c.ByWeek) != 1 || c.ByWeek[0].Week != evidenceWeekKey(t0) || c.ByWeek[0].Passes["2"] != 2 || c.ByWeek[0].InFlight != 2 {
		t.Errorf("by_week = %+v", c.ByWeek)
	}
	// A trail that scanned past the cap without a green is "4+".
	capped := CIRecoveryRollup{PRs: map[string]CIRecoveryTrail{"o/r#4": {Trail: ghpkg.PRCITrail{State: "open", Evaluated: 4, ScanCapped: true}}}}
	if x := buildEvidenceCIRecovery(claims, merges, capped, win, "", EvidenceCIPolicy{}); x.Convergence.Passes["4+"] != 1 {
		t.Errorf("capped scan = %v", x.Convergence.Passes)
	}
	if r.MedianPushesToGreen == nil || *r.MedianPushesToGreen != 0.5 || r.MedianHoursToGreen == nil || *r.MedianHoursToGreen != 1.5 {
		t.Errorf("medians = %v %v, want pushes 0.5 (0 and 1) hours 1.5 (1h and 2h)", r.MedianPushesToGreen, r.MedianHoursToGreen)
	}
	if !got.Policy.SelfAuthoredAutoMerge {
		t.Error("policy must be echoed")
	}
	// repo filter and window
	if x := buildEvidenceCIRecovery(claims, merges, rollup, win, "x/y", EvidenceCIPolicy{}); x.N != 0 || x.Red.N != 0 || x.Gate.MergedN != 0 {
		t.Errorf("other repo = %+v", x)
	}
	if x := buildEvidenceCIRecovery(claims, merges, rollup, evidenceWindow{Hours: 1, From: now.Add(-time.Hour), To: now}, "", EvidenceCIPolicy{}); x.N != 0 || x.Gate.MergedN != 0 {
		t.Errorf("window by open time = %+v", x)
	}
}
