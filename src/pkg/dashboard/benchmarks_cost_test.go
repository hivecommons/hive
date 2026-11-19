package dashboard

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	"github.com/hivecommons/hive/pkg/tokens"
)

func TestBenchmarksCostLookupFromLaunchCost(t *testing.T) {
	if benchmarksCostLookup(nil) != nil {
		t.Fatal("nil collector must disable attribution")
	}
	summary, launches, _ := launchFixture(t)
	lc := collect.NewLaunchCostCollector(&fakeLaunchTokens{summary: summary, launches: launches}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lc.Start(ctx)

	lookup := benchmarksCostLookup(lc)
	// Reviewer record: PR ref as the swarm spells it.
	c, ok := lookup([]benchmarks.CostRef{{Ref: "o/r#9", Kind: tokens.LaunchRefKindPR}, {Ref: "r#9", Kind: tokens.LaunchRefKindPR}})
	if !ok || c.Basis != benchmarks.CostBasisTokensXPricing || c.USDEst == nil || *c.USDEst <= 0 {
		t.Fatalf("pr ref not attributed: %+v", c)
	}
	if c.Launches != 1 || c.PriceTableDate != tokens.PriceTableDate() {
		t.Fatalf("launches/price table: %+v", c)
	}
	if c.InputTokens == nil || *c.InputTokens != 50 || c.OutputTokens == nil || *c.OutputTokens != 5 {
		t.Fatalf("tokens from snapshot: in=%v out=%v", c.InputTokens, c.OutputTokens)
	}
	// Author record closing issue 1: governor ref, issue kind.
	c, ok = lookup([]benchmarks.CostRef{{Ref: "o/r#1", Kind: tokens.LaunchRefKindIssue}})
	if !ok || *c.USDEst <= 0 || *c.InputTokens != 200 {
		t.Fatalf("issue ref not attributed: %+v", c)
	}
	// Unknown ref: nothing landed.
	if c, ok = lookup([]benchmarks.CostRef{{Ref: "o/r#99", Kind: tokens.LaunchRefKindIssue}}); ok || c.Basis != benchmarks.CostBasisUnknown {
		t.Fatalf("unrelated ref must be unknown: %+v", c)
	}
}
