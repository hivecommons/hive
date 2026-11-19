package delegatedecision

import (
	"strings"
	"testing"
)

// governor.labels.stage.abandon_after: a contributor PR the idle clock
// flagged abandoned gets a recommendation to close it as stale or have a
// person take it over. Recommend-only: it never closes, adopts or comments
// a decision.
func TestDecideAbandonedRecommendsOnly(t *testing.T) {
	adoptionStalled := Input{Kind: KindPR, Number: 731, Author: "contributor",
		Reasons: []string{"adoption-stalled:contributor-lane:workflow-path"}, AdoptionStalled: true, WorkflowPath: true,
		Abandoned: true, AbandonedReason: "abandoned 9d: contributor-lane:workflow-path"}
	needsAuthor := Input{Kind: KindPR, Number: 653, Author: "contributor", HumanAuthor: true,
		Reasons:   []string{"check-failing:unit"},
		Abandoned: true, AbandonedReason: "abandoned 10d: check-failing:unit"}
	for name, in := range map[string]Input{"085 adoption-stalled": adoptionStalled, "071 silent author": needsAuthor} {
		t.Run(name, func(t *testing.T) {
			d := Run(in)
			if d.Rule != RuleAbandonedStale {
				t.Fatalf("rule = %s, want %s: %q", d.Rule, RuleAbandonedStale, d.Text)
			}
			if d.Verdict != VerdictRecommendOnly || !d.KeepsPark {
				t.Fatalf("verdict %s keepsPark %v, want recommend-only keeping the park", d.Verdict, d.KeepsPark)
			}
			for _, a := range d.Actions {
				if a.Kind != ActionRecommendOnly {
					t.Fatalf("action %s: the abandoned rule must never act, only recommend", a.Kind)
				}
			}
			if !strings.Contains(d.Text, "close") || !strings.Contains(d.Text, "take it over") || !strings.Contains(d.Rationale, in.AbandonedReason) {
				t.Fatalf("text %q / rationale %q", d.Text, d.Rationale)
			}
		})
	}
}

// Without the flag (setting off, below the threshold, or activity cleared
// it) the rule never fires; nor for a hive PR or an author whose relay
// picked the rework up.
func TestDecideAbandonedOnlyForFlaggedContributorPRs(t *testing.T) {
	base := Input{Kind: KindPR, Number: 740, Author: "contributor",
		Reasons: []string{"adoption-stalled:check-failing:unit"}, AdoptionStalled: true,
		Abandoned: true, AbandonedReason: "abandoned 9d: check-failing:unit"}
	for name, in := range map[string]Input{
		"not flagged":        with(base, func(i *Input) { i.Abandoned, i.AbandonedReason = false, "" }),
		"hive-authored":      with(base, func(i *Input) { i.HiveAuthored = true }),
		"rework picked up":   with(base, func(i *Input) { i.ReworkPickedUp = true }),
		"issue, not a PR":    with(base, func(i *Input) { i.Kind = KindIssue }),
		"flagged but no why": with(base, func(i *Input) { i.AbandonedReason = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			d := Run(in)
			if name == "flagged but no why" {
				if d.Rule != RuleAbandonedStale || d.Verdict != VerdictRecommendOnly {
					t.Fatalf("rule %s verdict %s", d.Rule, d.Verdict)
				}
				return
			}
			if d.Rule == RuleAbandonedStale {
				t.Fatalf("abandoned rule fired: %q", d.Text)
			}
		})
	}
}

func TestGlossaryExplainsAbandonedStale(t *testing.T) {
	e, ok := Explain(RuleAbandonedStale)
	if !ok || e.Kind != GlossaryRule || !strings.Contains(e.Plain, "close it as stale or take it over") {
		t.Fatalf("Explain(%s) = %+v %v, want the glossary entry", RuleAbandonedStale, e, ok)
	}
}
