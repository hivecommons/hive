package config

import "testing"

// governor.labels.stage.adoption.maintainer_policy (hivecommons/hive#14524):
// adopt by default — clanker PRs are adoptable on disconnect regardless of
// the owner's repo role — with ask and never as opt-outs; anything else
// falls back to the default.
func TestStageAdoptionMaintainerPolicy(t *testing.T) {
	if DefaultAdoptionMaintainerPolicy != AdoptionMaintainerPolicyAdopt {
		t.Fatalf("default = %q, want adopt (owner decision 2026-10-28)", DefaultAdoptionMaintainerPolicy)
	}
	for in, want := range map[string]string{
		"": "adopt", "adopt": "adopt", " ADOPT ": "adopt",
		"ask": "ask", "Ask": "ask", "never": "never", " never\n": "never",
		"always": "adopt", "no": "adopt",
	} {
		if got := (StageAdoptionConfig{MaintainerPolicy: in}).EffectiveMaintainerPolicy(); got != want {
			t.Errorf("EffectiveMaintainerPolicy(%q) = %q, want %q", in, got, want)
		}
	}
	if len(AdoptionMaintainerPolicies) != 3 {
		t.Fatalf("policies = %v", AdoptionMaintainerPolicies)
	}
}
