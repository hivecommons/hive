package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/tokens"
)

func TestApplyCoinEstimateAddsAPIShape(t *testing.T) {
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	s.deps.Config.Governor.Budget.Coins = map[string]config.CoinBudgetConfig{
		"bob": {TokensPerCoin: 500000, USDPerCoin: 0.50, Label: "BC", Budget: 10},
	}
	est := costEstimated{
		ByAgent:   []costModelEntry{{Name: "scanner"}, {Name: "quality"}},
		BySession: []costSessionEntry{{SessionID: "s1"}, {SessionID: "s2"}},
	}
	s.applyCoinEstimate(&tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		{SessionID: "s1", Agent: "scanner", Backend: tokens.BackendBob, TotalTokens: 250000},
		{SessionID: "s2", Agent: "quality", Backend: tokens.BackendClaude, TotalTokens: 250000},
	}}, &est)

	if est.CoinLabel != "BC" || est.Coins != 0.5 {
		t.Fatalf("coin total/label = %v/%q, want 0.5/BC", est.Coins, est.CoinLabel)
	}
	if est.CoinBudget == nil || *est.CoinBudget != 10 || est.CoinsRemaining == nil || *est.CoinsRemaining != 9.5 {
		t.Fatalf("coin budget fields = budget %v remaining %v", est.CoinBudget, est.CoinsRemaining)
	}
	if est.ByAgent[0].Coins != 0.5 || est.ByAgent[1].Coins != 0 {
		t.Fatalf("per-agent coins = %+v", est.ByAgent)
	}
	if est.BySession[0].Coins != 0.5 || est.BySession[1].Coins != 0 {
		t.Fatalf("per-session coins = %+v", est.BySession)
	}
}
