package delegatedecision

import "testing"

// hivecommons/hive#18872: the ladder and the contributor nudge agree on one
// outcome per head. Rung 5 fires exactly when the author is unreachable
// (the nudge is withheld under that condition) and never for a reachable
// author (who keeps the nudge and the real grace).
func TestLadderAndNudgeAgree(t *testing.T) {
	base := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 18561, AuthorTooling: AuthorToolingClanker}
	patterns := []struct {
		name string
		mut  func(*Input)
	}{
		{"conflicting", func(i *Input) { i.Conflicting = true; i.CIGreen = true }},
		{"check-failing", func(i *Input) { i.CIGreen = false }},
		{"dco-missing", func(i *Input) { i.CIGreen = true; i.DCOMissing = true }},
		{"adoption-stalled", func(i *Input) { i.CIGreen = true; i.AdoptionStalled = true }},
	}
	for _, p := range patterns {
		t.Run(p.name+"/unreachable", func(t *testing.T) {
			in := with(base, p.mut)
			if in.ContributorReachable() || !AdoptsImmediately(in) {
				t.Fatalf("an unreachable author's head is superseded now (reachable=%v)", in.ContributorReachable())
			}
		})
		for _, reach := range []struct {
			name string
			mut  func(*Input)
		}{
			{"online", func(i *Input) { i.ContributorOnline = true }},
			{"rework-picked-up", func(i *Input) { i.ReworkPickedUp = true }},
		} {
			t.Run(p.name+"/"+reach.name, func(t *testing.T) {
				in := with(with(base, p.mut), reach.mut)
				if !in.ContributorReachable() || AdoptsImmediately(in) {
					t.Fatalf("a reachable author gets a real chance, not rung 5")
				}
			})
		}
	}
}
