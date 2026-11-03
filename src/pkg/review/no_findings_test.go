package review

import "testing"

func TestNoFindingsAcknowledgement(t *testing.T) {
	yes := []string{
		"**Reviewed** — no findings from this perspective.",
		"**Reviewed** — no findings from security, correctness.",
		"**Reviewed** — no findings from adjudicator (all 2 review heads agreed: abc1234, def5678).",
		"  Reviewed - no findings from this perspective  \n",
		"_Reviewed_ – no findings from maintainability",
	}
	for _, b := range yes {
		if !NoFindingsAcknowledgement(b) {
			t.Errorf("should match: %q", b)
		}
	}
	no := []string{
		"",
		"**Reviewed** — no findings from security.\n\n**correctness**\n- src/x.go:12 nil deref",
		"**security**\n- src/x.go:12 nil deref\n\n_No findings from: style._",
		"No findings from this perspective, but one nit: …",
	}
	for _, b := range no {
		if NoFindingsAcknowledgement(b) {
			t.Errorf("should not match: %q", b)
		}
	}
}
