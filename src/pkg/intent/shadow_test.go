package intent

import "testing"

func TestVerdictBlockReasonAndShadowConfidence(t *testing.T) {
	unauth := Verdict{Tier: Tier1, AgentPR: true, Reason: ReasonTier1NeedsIssue}
	if got := unauth.BlockReason(); got != ReasonTier1NeedsIssue {
		t.Fatalf("BlockReason = %q, want %q", got, ReasonTier1NeedsIssue)
	}
	if got := unauth.ShadowConfidence(); got != 1 {
		t.Fatalf("ShadowConfidence = %v, want 1 without a model verdict", got)
	}
	mis := Verdict{Tier: Tier1, AgentPR: true, Authorized: true, Reason: ReasonLinkedIssue,
		Alignment: &AlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "drift", Model: &ModelAlignmentVerdict{Confidence: 0.4}}}
	if got := mis.BlockReason(); got != ReasonAlignmentMisaligned+": drift" {
		t.Fatalf("BlockReason = %q, want alignment rationale", got)
	}
	if got := mis.ShadowConfidence(); got != 0.4 {
		t.Fatalf("ShadowConfidence = %v, want model confidence 0.4", got)
	}
	mis.Alignment.Model.Confidence = 7
	if got := mis.ShadowConfidence(); got != 1 {
		t.Fatalf("ShadowConfidence = %v, want 1 for out-of-range model confidence", got)
	}
}
