package config

import (
	"strings"
	"testing"
)

func TestBobCoinBudgetDefaultsAndEnv(t *testing.T) {
	t.Setenv(BobTokensPerCoinEnvVar, "250000")
	t.Setenv(BobUSDPerCoinEnvVar, "0.25")
	t.Setenv(BobCoinBudgetEnvVar, "42")
	t.Setenv(BobUSDBudgetEnvVar, "21")

	cfg := &Config{}
	cfg.applyCoinBudgetDefaults()

	bob, ok := cfg.Governor.Budget.CoinConfig("bob")
	if !ok {
		t.Fatal("bob coin conversion not configured")
	}
	if bob.TokensPerCoin != 250000 || bob.USDPerCoin != 0.25 || bob.Budget != 42 || bob.Label != DefaultBobCoinLabel {
		t.Fatalf("bob coin defaults/env = %+v", bob)
	}
	if cfg.Governor.Budget.USD != 21 {
		t.Fatalf("usd budget = %v, want 21", cfg.Governor.Budget.USD)
	}
	if got := bob.TokensToCoins(500000); got != 2 {
		t.Fatalf("TokensToCoins = %v, want 2", got)
	}
	if got := bob.CoinsToUSD(2); got != 0.5 {
		t.Fatalf("CoinsToUSD = %v, want 0.5", got)
	}
}

func TestBobCoinBudgetValidation(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "hivecommons"},
		GitHub:  GitHubConfig{Token: "ghp_test"},
		Agents:  map[string]AgentConfig{"scanner": {Enabled: true, Backend: "bob"}},
	}
	cfg.applyDefaults()
	cfg.Governor.Budget.Coins["bob"] = CoinBudgetConfig{TokensPerCoin: -1, USDPerCoin: DefaultBobUSDPerCoin, Label: DefaultBobCoinLabel}
	err := cfg.ValidateWithOptions(ValidateOptions{RequireAgents: true})
	if err == nil || !strings.Contains(err.Error(), "tokens_per_coin must be positive") {
		t.Fatalf("ValidateWithOptions error = %v, want tokens_per_coin validation", err)
	}

	cfg.Governor.Budget.Coins["bob"] = CoinBudgetConfig{TokensPerCoin: DefaultBobTokensPerCoin, USDPerCoin: DefaultBobUSDPerCoin, Label: DefaultBobCoinLabel}
	cfg.Governor.Budget.USD = -1
	err = cfg.ValidateWithOptions(ValidateOptions{RequireAgents: true})
	if err == nil || !strings.Contains(err.Error(), "budget.usd must be non-negative") {
		t.Fatalf("ValidateWithOptions error = %v, want budget.usd validation", err)
	}
}
