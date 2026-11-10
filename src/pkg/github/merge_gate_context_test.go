package github

import "testing"

func TestIsMergeGateContext(t *testing.T) {
	for name, want := range map[string]bool{
		"tide": true, " tide ": true, "enforce-guardrails": true, "dco": true, "DCO": true,
		"build": false, "test": false, "Test (chromium, shard 1)": false, "netlify/site/deploy-preview": false, "": false,
	} {
		if got := IsMergeGateContext(name); got != want {
			t.Errorf("IsMergeGateContext(%q) = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"tide", "enforce-guardrails"} {
		if !isMetaCheck(name) {
			t.Errorf("isMetaCheck(%q) lost the Prow merge gate", name)
		}
	}
}
