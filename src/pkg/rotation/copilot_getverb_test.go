package rotation

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCopilotProbeUsesOnlyGetVerb is a runtime, verb-level pin: whatever gh
// invocations a Copilot probe issues, every one must be a plain read. It is the
// complement of TestNoCopilotBudgetMutation's static scan — the scan catches a
// hardcoded budgets URL in the tree, this catches a mutating request assembled
// at runtime and sent anywhere. The consequence of a misread here is a bill
// (#6980, inherited from #6833), so both guards are kept independent: this one
// survives a refactor that reorganizes the budget test.
//
// It also closes a gap the landed budget test's runtime block leaves open. That
// block rejects -X/--method/--input, but `gh api` is also turned into a POST by
// a request-body field flag (-f/-F/--field/--raw-field), which it does not
// check. This test rejects those flags and any mutating verb value as well, so
// no code path can spend a premium request or mutate spend through a field
// param. Builds on the landed scanner adapter (#6997); adds the missing
// verb-level runtime assertion from #6980.
func TestCopilotProbeUsesOnlyGetVerb(t *testing.T) {
	var calls [][]string
	p := CopilotProber{
		ThresholdPct:     85,
		MonthlyAllowance: 1000,
		Run:              copilotFakeGH(t, &calls, string(copilotFixture(t)), nil),
		Now:              func() time.Time { return sep2026 },
	}
	if h := p.Probe(context.Background()); h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v, want a clean probe so the recorded calls are the real ones", h.ProbeErr)
	}
	if len(calls) == 0 {
		t.Fatal("probe issued no gh calls; nothing to verify")
	}

	// Flags that either override the HTTP verb or attach a request body, both
	// of which make `gh api` a write.
	writeFlags := map[string]bool{
		"-X": true, "--method": true,
		"-f": true, "-F": true, "--field": true, "--raw-field": true,
		"--input": true,
	}
	mutatingVerbs := map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

	for _, call := range calls {
		if len(call) < 2 || call[0] != "gh" || call[1] != "api" {
			t.Errorf("probe issued %v — only `gh api` GET reads are allowed", call)
			continue
		}
		for _, arg := range call[2:] {
			if writeFlags[arg] {
				t.Errorf("probe issued %v carrying write flag %q — a reading must never mutate billing or spend a premium request", call, arg)
			}
			if mutatingVerbs[strings.ToUpper(arg)] {
				t.Errorf("probe issued %v carrying mutating verb %q", call, arg)
			}
		}
	}
}
