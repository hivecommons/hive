package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCoinBudgetExhaustedSuppressesKicks(t *testing.T) {
	g := New(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1m"}},
		},
	}, map[string]config.AgentConfig{"scanner": {Enabled: true}}, nil)
	g.SetCoinBudget(1, "BC")
	g.SeedBudget(0, nil, nil, time.Now())
	g.UpdateBudgetFromTotalsAndCoins(0, nil, nil, 2)

	if due := g.Evaluate(1, 0, 0, 0); len(due) != 0 {
		t.Fatalf("coin-exhausted budget should suppress kicks, got %v", due)
	}
	if !g.GetState().BudgetExhausted {
		t.Fatal("state should report budget exhausted for coin cap")
	}
}

func TestCoinBudgetLevelReportsExhaustion(t *testing.T) {
	g := New(config.GovernorConfig{}, nil, nil)
	g.SetCoinBudget(10, "BC")
	g.SeedBudget(0, nil, nil, time.Now())
	trans := g.UpdateBudgetFromTotalsAndCoins(0, nil, nil, 10)
	if !trans.ExhaustedActive || !trans.ExhaustedCrossed {
		t.Fatalf("coin cap exhaustion transitions = %+v", trans)
	}
}

func TestUSDBudgetExhaustedSuppressesKicks(t *testing.T) {
	g := New(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1m"}},
		},
	}, map[string]config.AgentConfig{"scanner": {Enabled: true}}, nil)
	g.SetUSDBudget(5)
	g.SeedBudget(0, nil, nil, time.Now())
	g.UpdateBudgetFromTotalsCoinsAndUSD(0, nil, nil, 0, 6)

	if due := g.Evaluate(1, 0, 0, 0); len(due) != 0 {
		t.Fatalf("usd-exhausted budget should suppress kicks, got %v", due)
	}
	b := g.GetBudget()
	if b.ExhaustedUnit != "usd" {
		t.Fatalf("exhausted unit = %q, want usd", b.ExhaustedUnit)
	}
}

func TestBothCoinAndUSDBudgetFirstExhaustedWins(t *testing.T) {
	g := New(config.GovernorConfig{}, nil, nil)
	g.SetCoinBudget(10, "BC")
	g.SetUSDBudget(5)
	g.SeedBudget(0, nil, nil, time.Now())
	g.UpdateBudgetFromTotalsCoinsAndUSD(0, nil, nil, 4, 5.5)
	if got := g.GetBudget().ExhaustedUnit; got != "usd" {
		t.Fatalf("exhausted unit after usd-only exhaustion = %q, want usd", got)
	}
	g.UpdateBudgetFromTotalsCoinsAndUSD(0, nil, nil, 11, 6)
	if got := g.GetBudget().ExhaustedUnit; got != "usd" {
		t.Fatalf("exhausted unit after later coin exhaustion = %q, want usd", got)
	}
}
