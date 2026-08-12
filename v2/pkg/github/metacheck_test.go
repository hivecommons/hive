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

// TestIsIgnorableCICheck locks in the wider non-required set that
// commitGreen uses to decide whether a bad CONCLUSION (cancelled, failure,
// ...) is allowed to pass: it includes everything isMetaCheck already
// covers, plus the browser/E2E suites that routinely get cancelled
// mid-flight (Playwright, Mobile Browser Tests, the chromium shard matrix).
// Required build/test/docker checks must NOT be in this set.
func TestIsIgnorableCICheck(t *testing.T) {
	ignorable := []string{
		"tide",
		"enforce-guardrails",
		"netlify/kubestellarconsole/deploy-preview",
		"copilot-pull-request-reviewer",
		"Playwright",
		"Mobile Browser Tests",
		"Test (chromium, shard 1)",
		"Test (chromium, shard 2/4)",
		"coverage-report",
	}
	for _, name := range ignorable {
		if !isIgnorableCICheck(name) {
			t.Errorf("isIgnorableCICheck(%q) = false, want true (non-required check)", name)
		}
	}
	required := []string{"build-gate", "build", "test", "docker", "unit-test", "TypeScript & ESLint"}
	for _, name := range required {
		if isIgnorableCICheck(name) {
			t.Errorf("isIgnorableCICheck(%q) = true, want false (required check must still gate)", name)
		}
	}
}
