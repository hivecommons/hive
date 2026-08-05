package github

import "testing"

// TestIsMetaCheck locks in that merge-gate and deploy-status checks are
// excluded from CI classification, so a stale enforce-guardrails run or a
// Netlify status mirror can never mark an otherwise-green PR as failing.
func TestIsMetaCheck(t *testing.T) {
	meta := []string{
		"tide",
		"enforce-guardrails",
		"Header rules - kubestellarconsole",
		"Pages changed - kubestellarconsole",
		"Redirect rules - kubestellarconsole",
		"netlify/kubestellarconsole/deploy-preview",
		"copilot-pull-request-reviewer",
	}
	for _, name := range meta {
		if !isMetaCheck(name) {
			t.Errorf("isMetaCheck(%q) = false, want true (merge-gate/deploy mirror)", name)
		}
	}
	code := []string{"build-gate", "coverage-gate", "unit-test", "TypeScript & ESLint", "Test (chromium, shard 1)"}
	for _, name := range code {
		if isMetaCheck(name) {
			t.Errorf("isMetaCheck(%q) = true, want false (real code CI must gate)", name)
		}
	}
}
