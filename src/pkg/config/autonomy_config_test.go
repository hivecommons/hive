package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAutonomyConfigDefaultsOmitWhenOff(t *testing.T) {
	data, err := yaml.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "autonomy:") {
		t.Fatalf("zero autonomy config should omit block, got:\n%s", data)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("{}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Autonomy.AutoPromote || cfg.Autonomy.AutoDemote {
		t.Fatalf("flags default on: %+v", cfg.Autonomy)
	}
	if cfg.Autonomy.EffectivePromoteAfter() != DefaultAutonomyPromoteAfter || cfg.Autonomy.EffectiveDemoteOn() != DefaultAutonomyDemoteOn || cfg.Autonomy.EffectiveCooldownDays() != DefaultAutonomyCooldownDays {
		t.Fatalf("defaults = %+v", cfg.Autonomy)
	}
}

func TestClearingSelfAuthorizationHoldPreservesACMMRepoPolicy(t *testing.T) {
	hold := true
	level := 3
	cfg := &Config{Project: ProjectConfig{
		Org: "hivecommons",
		RepoPolicies: []RepoPolicy{{
			Repo:                  "hive",
			SelfAuthorizationHold: &hold,
			ACMMLevel:             &level,
			ACMMPinned:            true,
		}},
	}}
	changed := cfg.SetSelfAuthorizationHoldForRepos(map[string]*bool{"hive": nil})
	if !changed {
		t.Fatal("clearing self-authorization hold did not report a change")
	}
	rp, ok := cfg.RepoPolicyFor("hive")
	if !ok {
		t.Fatal("repo policy was removed")
	}
	if rp.SelfAuthorizationHold != nil || rp.ACMMLevel == nil || *rp.ACMMLevel != level || !rp.ACMMPinned {
		t.Fatalf("repo policy after clear = %+v", rp)
	}
}
