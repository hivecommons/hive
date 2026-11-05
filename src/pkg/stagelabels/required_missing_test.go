package stagelabels

import (
	"strings"
	"testing"
)

func TestRouteHumanMissingRequiredChecksIsAPendingGate(t *testing.T) {
	e := Evidence{Head: "0c56fd61c0000000000000000000000000000000", Verdict: VerdictApproved, Cause: CauseTrust,
		Checks: CheckSummary{Known: true, Total: 4, Green: 4, Missing: []string{"contract"}}, DCO: DCOYes}
	r := RouteHuman(e)
	if r.Stage != Reviewed {
		t.Fatalf("stage = %v, want Reviewed", r.Stage)
	}
	if len(r.Reasons) == 0 || !strings.Contains(r.Reasons[0], "`contract` not reported on this head") {
		t.Fatalf("reasons = %q", r.Reasons)
	}
	if !strings.Contains(r.Summary, "4/4 checks green, required not reported: contract") {
		t.Fatalf("summary = %q", r.Summary)
	}
	e.Checks.Missing = nil
	if r := RouteHuman(e); r.Stage != ReadyForHumanMerge {
		t.Fatalf("stage without missing checks = %v, want 090", r.Stage)
	}
}
