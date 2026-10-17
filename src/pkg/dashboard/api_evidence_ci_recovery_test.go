package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestBuildEvidenceCIRecovery pins panel 5's recovery read: the claim
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
	bundles := []*evidence.Bundle{b1, b1m, b2, b3, b3c, b4, b5a, b5b, b6, b7, b8, b8b}
	claims := evidenceCIClaims(bundles, evidence.FoldPRs(bundles, "", ""))
	if c := claims["o/r#5"]; c == nil || c.state != evidenceCIGreen || c.claimHead != "e2" {
		t.Fatalf("claim 5 = %+v (first CAPTURED snapshot is the claim)", c)
	}
	if _, ok := claims["o/r#6"]; ok {
		t.Fatal("human PR must not be a claim")
	}
	rollup := CIRecoveryRollup{PRs: map[string]CIRecoveryTrail{
		"o/r#1": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(3 * time.Hour), Heads: []ghpkg.PRCIHead{{SHA: "a1", At: t0, State: "red"}, {SHA: "a2", At: t0.Add(2 * time.Hour), State: "green"}}}},
		"o/r#2": {Trail: ghpkg.PRCITrail{State: "merged", MergedAt: t0.Add(time.Hour), Heads: []ghpkg.PRCIHead{{SHA: "b1", At: t0, State: "green"}}}},
		"o/r#7": {Trail: ghpkg.PRCITrail{State: "open", Heads: []ghpkg.PRCIHead{{SHA: "g1", At: t0, State: "red"}}}},
	}}
	win := evidenceWindow{Hours: 168, From: now.Add(-168 * time.Hour), To: now}
	got := buildEvidenceCIRecovery(claims, rollup, win, "", EvidenceCIPolicy{SelfAuthoredAutoMerge: true})
	if got.N != 7 || got.AtClaim["red"] != 6 || got.AtClaim["green"] != 1 || got.AtClaim["none"] != 0 {
		t.Errorf("n/at_claim = %d %+v", got.N, got.AtClaim)
	}
	r := got.Red
	if r.N != 6 || r.RecoveredMerged != 2 || r.GreenOpen != 1 || r.Abandoned != 1 || r.Pending != 1 || r.OutcomeUnknown != 1 {
		t.Errorf("red = %+v", r)
	}
	if r.Enriched != 3 || r.NotEnriched != 3 || r.SameHeadRecovered != 1 {
		t.Errorf("enrichment = %+v", r)
	}
	if r.MedianPushesToGreen == nil || *r.MedianPushesToGreen != 0.5 || r.MedianHoursToGreen == nil || *r.MedianHoursToGreen != 1.5 {
		t.Errorf("medians = %v %v, want pushes 0.5 (0 and 1) hours 1.5 (1h and 2h)", r.MedianPushesToGreen, r.MedianHoursToGreen)
	}
	if !got.Policy.SelfAuthoredAutoMerge {
		t.Error("policy must be echoed")
	}
	// repo filter and window
	if x := buildEvidenceCIRecovery(claims, rollup, win, "x/y", EvidenceCIPolicy{}); x.N != 0 || x.Red.N != 0 {
		t.Errorf("other repo = %+v", x)
	}
	if x := buildEvidenceCIRecovery(claims, rollup, evidenceWindow{Hours: 1, From: now.Add(-time.Hour), To: now}, "", EvidenceCIPolicy{}); x.N != 0 {
		t.Errorf("window by open time = %+v", x)
	}
}
