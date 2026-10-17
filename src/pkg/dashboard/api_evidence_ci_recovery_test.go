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
	// 9: human PR merged while red → gate breach
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
	bundles := []*evidence.Bundle{b1, b1m, b2, b3, b3c, b4, b5a, b5b, b6, b7, b8, b8b, b9, b10, b11, b11m}
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
		"o/r#1":  {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(3 * time.Hour), Evaluated: 2, PassesToGreen: 2, Heads: []ghpkg.PRCIHead{{SHA: "a1", At: t0, State: "red"}, {SHA: "a2", At: t0.Add(2 * time.Hour), State: "green"}}}},
		"o/r#2":  {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), Evaluated: 1, PassesToGreen: 1, Heads: []ghpkg.PRCIHead{{SHA: "b1", At: t0, State: "green"}}}},
		"o/r#7":  {Trail: ghpkg.PRCITrail{State: "open", Evaluated: 1, Heads: []ghpkg.PRCIHead{{SHA: "g1", At: t0, State: "red"}}}},
		"o/r#10": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(4 * time.Hour), Heads: []ghpkg.PRCIHead{{SHA: "j1", At: t0, State: "red"}, {SHA: "j2", At: t0.Add(time.Hour), State: "green"}}}},
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
	// check state; unknown is outside the denominator, never a breach.
	g := got.Gate
	if g.MergedN != 4 || g.WithChecks != 3 || g.Green != 2 || g.Breaches != 1 || g.Pending != 0 || g.Unknown != 1 {
		t.Errorf("gate = %+v", g)
	}
	if len(g.BreachPRs) != 1 || g.BreachPRs[0].Number != 9 || g.BreachPRs[0].State != "red" || g.BreachPRs[0].Author != "human" || g.BreachPRs[0].Source != "bundle" {
		t.Errorf("breach list = %+v", g.BreachPRs)
	}
	if g.GreenToMergeN != 1 || g.MedianGreenToMergeHours == nil || *g.MedianGreenToMergeHours != 3 {
		t.Errorf("gate cost = %d %v, want 1 PR at 3h (trail green → merge)", g.GreenToMergeN, g.MedianGreenToMergeHours)
	}
	// A required check the snapshot never saw green is a missing_required
	// breach; a trail-sourced state has no check names, so it is not judged.
	strict := buildEvidenceCIRecovery(claims, merges, rollup, win, "", EvidenceCIPolicy{RequiredChecks: []string{"build", "lint"}})
	if strict.Gate.Breaches != 2 || strict.Gate.Green != 1 || len(strict.Gate.BreachPRs) != 2 || strict.Gate.BreachPRs[0].State != "missing_required" || strict.Gate.BreachPRs[0].Missing[0] != "lint" {
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
