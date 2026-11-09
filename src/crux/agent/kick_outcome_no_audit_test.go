package agent

import "testing"

// TestClassifyTurnTail_SecCheckNoAuditStandsDown pins the contract the
// sec-check templates rely on: a turn that ends on
// "STAND DOWN — no repository audited: …" is recorded as a stand-down
// (blocked), and the template sentence that describes the line, which starts
// mid-sentence, is not.
func TestClassifyTurnTail_SecCheckNoAuditStandsDown(t *testing.T) {
	ended := []string{
		"● Local audit blocked: no authorized repository checkout, and GitHub reads were refused.",
		"  Security posture remains unassessed.",
		"● STAND DOWN — no repository audited: no checkout and GitHub reads refused (429 hourly cap reached)",
		"❯",
	}
	if kind, reason := classifyTurnTail(ended); kind != KickOutcomeStandDown {
		t.Fatalf("no-audit turn classified %q (reason %q), want %q", kind, reason, KickOutcomeStandDown)
	}

	instruction := []string{
		"   **If you audited no repository this turn** (no checkout and the clone failed or was refused), do not describe a posture: make the last line `STAND DOWN — no repository audited: <what stopped you>`.",
	}
	if kind, _ := classifyTurnTail(instruction); kind == KickOutcomeStandDown {
		t.Fatal("the template sentence describing the stand-down line was itself read as a stand-down")
	}
}
