package intent

import "testing"

// BlocksMerge is the single refusal predicate shared by the human merge-queue
// lane (writeMergeEligible) and the App self-merge sweep (#6258).
func TestVerdictBlocksMerge(t *testing.T) {
	tests := []struct {
		name    string
		verdict Verdict
		enforce bool
		want    bool
	}{
		{name: "unauthorized agent PR enforced", verdict: Verdict{Authorized: false, AgentPR: true}, enforce: true, want: true},
		{name: "unauthorized agent PR advisory", verdict: Verdict{Authorized: false, AgentPR: true}, enforce: false, want: false},
		{name: "authorized agent PR enforced", verdict: Verdict{Authorized: true, AgentPR: true}, enforce: true, want: false},
		{name: "unauthorized human PR never blocks", verdict: Verdict{Authorized: false, AgentPR: false}, enforce: true, want: false},
		{name: "misaligned agent PR enforced", verdict: Verdict{Authorized: true, AgentPR: true, Alignment: &AlignmentVerdict{Status: AlignmentStatusMisaligned}}, enforce: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.verdict.BlocksMerge(tt.enforce); got != tt.want {
				t.Fatalf("BlocksMerge(%v) = %v, want %v", tt.enforce, got, tt.want)
			}
		})
	}
}
