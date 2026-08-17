package reach

import (
	"math"
	"testing"
	"time"
)

type fakeAncestryMap struct {
	ancestry map[string]map[string]bool
}

func (f *fakeAncestryMap) IsAncestor(ancestor, descendant string) (bool, error) {
	if f.ancestry == nil {
		return false, nil
	}
	if ancestor == descendant {
		return true, nil
	}
	return f.ancestry[ancestor][descendant], nil
}

func TestComputeErrorDelta_MeasuredAndDelta(t *testing.T) {
	anc := &fakeAncestryMap{
		ancestry: map[string]map[string]bool{
			"c-merge": {"c-post": true},
			"c-pre":   {"c-post": true},
		},
	}

	reports := [][]ComponentReach{
		{
			// Pre-deploy commit: 100 spans, 2 errors -> 2.0% error rate
			{
				Component:  "governor",
				Commit:     "c-pre",
				SpansTotal: 100,
				SpansError: 2,
				FirstSeen:  time.Now().Add(-48 * time.Hour),
				LastSeen:   time.Now().Add(-24 * time.Hour),
			},
			// Post-deploy commit: 200 spans, 10 errors -> 5.0% error rate
			{
				Component:  "governor",
				Commit:     "c-post",
				SpansTotal: 200,
				SpansError: 10,
				FirstSeen:  time.Now().Add(-12 * time.Hour),
				LastSeen:   time.Now(),
			},
		},
	}

	delta := ComputeErrorDelta([]string{"governor"}, "c-merge", "c-post", anc, reports)

	if !delta.Measured {
		t.Fatalf("expected Measured=true, got false")
	}
	if math.Abs(delta.PreDeployRate-0.02) > 1e-6 {
		t.Errorf("PreDeployRate = %f, want 0.02", delta.PreDeployRate)
	}
	if math.Abs(delta.PostDeployRate-0.05) > 1e-6 {
		t.Errorf("PostDeployRate = %f, want 0.05", delta.PostDeployRate)
	}
	if math.Abs(delta.Delta-0.03) > 1e-6 {
		t.Errorf("Delta = %f, want +0.03", delta.Delta)
	}
	if delta.SpansPreTotal != 100 || delta.SpansPostTotal != 200 {
		t.Errorf("raw spans mismatch: pre=%d post=%d", delta.SpansPreTotal, delta.SpansPostTotal)
	}
}

func TestComputeErrorDelta_NeverFabricateContract(t *testing.T) {
	anc := &fakeAncestryMap{
		ancestry: map[string]map[string]bool{
			"c-merge": {"c-post": true},
		},
	}

	// Only post-deploy spans, zero pre-deploy spans
	reportsPostOnly := [][]ComponentReach{
		{
			{
				Component:  "governor",
				Commit:     "c-post",
				SpansTotal: 100,
				SpansError: 1,
			},
		},
	}

	delta := ComputeErrorDelta([]string{"governor"}, "c-merge", "c-post", anc, reportsPostOnly)
	if delta.Measured {
		t.Errorf("expected Measured=false when pre-deploy spans are missing (never-fabricate contract)")
	}

	// Empty components list
	deltaEmpty := ComputeErrorDelta([]string{}, "c-merge", "c-post", anc, reportsPostOnly)
	if deltaEmpty.Measured {
		t.Errorf("expected Measured=false for empty components")
	}
}

func TestComputePRReachRate(t *testing.T) {
	// Empty reports
	rate, measured := ComputePRReachRate(nil)
	if measured || rate != 0 {
		t.Errorf("expected (0, false) for empty reports, got (%f, %v)", rate, measured)
	}

	// 2 deployed attributable PRs, 1 reached
	reports := []PRReachReport{
		{
			PR:          1,
			Deployed:    true,
			Attribution: Attribution{Components: []string{"governor"}},
			ReachCount:  1,
		},
		{
			PR:          2,
			Deployed:    true,
			Attribution: Attribution{Components: []string{"proxy"}},
			ReachCount:  0,
		},
		{
			PR:          3,
			Deployed:    false, // not deployed, ignored
			Attribution: Attribution{Components: []string{"governor"}},
		},
		{
			PR:          4,
			Deployed:    true,
			Attribution: Attribution{Components: []string{}}, // docs-only, ignored
		},
	}

	rate, measured = ComputePRReachRate(reports)
	if !measured {
		t.Fatalf("expected measured=true")
	}
	if math.Abs(rate-0.5) > 1e-6 {
		t.Errorf("PRReachRate = %f, want 0.5 (50%%)", rate)
	}
}
