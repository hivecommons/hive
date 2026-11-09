package knowledge

import "testing"

// The task-repo boost orders facts within a type and never across types, and
// it leaves the confidence a kick shows alone.
func TestBoostTaskRepos_OrdersWithinTypeOnly(t *testing.T) {
	p := &Primer{config: PrimerConfig{Priority: []string{"gotcha", "pattern"}}}
	facts := []Fact{
		{Slug: "other-gotcha", Type: FactGotcha, Confidence: 0.50, Tags: []string{"repo:org/other"}},
		{Slug: "task-gotcha", Type: FactGotcha, Confidence: 0.45, Tags: []string{"repo:Org/Task"}},
		{Slug: "task-pattern", Type: FactPattern, Confidence: 0.90, Tags: []string{"repo:org/task"}},
		{Slug: "task-codemap", Type: FactPattern, Confidence: 0.85, Tags: []string{"repo:task"}},
		{Slug: "untagged", Type: FactPattern, Confidence: 0.92},
	}
	sorted := p.applyPriority(boostTaskRepos(facts, []string{"org/task"}))

	want := []string{"task-gotcha", "other-gotcha", "task-pattern", "task-codemap", "untagged"}
	for i, slug := range want {
		if sorted[i].Slug != slug {
			t.Fatalf("position %d = %s, want %s (order %v)", i, sorted[i].Slug, slug, slugsOf(sorted))
		}
	}
	for _, f := range sorted {
		if f.Slug == "task-gotcha" && f.Confidence != 0.45 {
			t.Errorf("boost changed the shown confidence to %.2f", f.Confidence)
		}
	}
}

func slugsOf(facts []Fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Slug
	}
	return out
}
