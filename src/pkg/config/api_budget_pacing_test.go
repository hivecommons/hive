package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// governor.api_budget_pacing is off unless set, survives a save and a load,
// and reaches the snapshot the sweep workers read.
func TestAPIBudgetPacingDefaultsOffAndRoundTrips(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("governor: {}"), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if cfg.Governor.APIBudgetPacing {
		t.Fatal("governor.api_budget_pacing must default to off")
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "api_budget_pacing") {
		t.Fatal("an unset governor.api_budget_pacing was written on save")
	}

	if err := yaml.Unmarshal([]byte("governor: {api_budget_pacing: true}"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Governor.APIBudgetPacing {
		t.Fatal("governor.api_budget_pacing: true was not loaded")
	}
	data, err = yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api_budget_pacing: true") {
		t.Fatalf("governor.api_budget_pacing: true was dropped on save:\n%s", data)
	}
	var restored Config
	if err := yaml.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Governor.APIBudgetPacing {
		t.Fatal("governor.api_budget_pacing did not survive a save and a load")
	}
	snap, err := cfg.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Governor.APIBudgetPacing {
		t.Fatal("the config snapshot the sweep workers read lost governor.api_budget_pacing")
	}
}
