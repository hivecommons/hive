package agentmode

// Tests for hivecommons/hive#8380: an agent posts an issue claim only at a
// level that may write comments on issues. Same ladder shape as the reviewer
// tier tests — every predicate is an ordinal comparison against the mode.

import "testing"

// CanComment turns on at ISSUES_ONLY and stays on above it. ADVISORY is the
// one rung that must not comment: its token has no Issues:write.
func TestCanComment_LadderStartsAtIssuesOnly(t *testing.T) {
	for _, tc := range []struct {
		mode AgentMode
		want bool
	}{
		{ModeAdvisory, false},
		{ModeIssuesOnly, true},
		{ModeIssuesAndPRs, true},
		{ModeIssuesPRsMerge, true},
	} {
		if got := tc.mode.CanComment(); got != tc.want {
			t.Errorf("%v.CanComment() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// CanComment must agree with the existing NeedsMCPWrite rung: both name the
// first mode that holds a write-capable token.
func TestCanComment_AgreesWithNeedsMCPWrite(t *testing.T) {
	for m := ModeAdvisory; m <= ModeIssuesPRsMerge; m++ {
		if m.CanComment() != m.NeedsMCPWrite() {
			t.Errorf("%v: CanComment=%v NeedsMCPWrite=%v — the write rung must be one place", m, m.CanComment(), m.NeedsMCPWrite())
		}
	}
}

// ModeForTokenTier inverts TokenTier for every mode, and maps the two
// tiers that are not a mode's own name (reviewer, merger) to the mode whose
// issue-write capability they actually carry.
func TestModeForTokenTier_InvertsTokenTier(t *testing.T) {
	for m := ModeAdvisory; m <= ModeIssuesPRsMerge; m++ {
		got, ok := ModeForTokenTier(m.TokenTier())
		if !ok || got != m {
			t.Errorf("ModeForTokenTier(%q) = %v,%v; want %v", m.TokenTier(), got, ok, m)
		}
	}
	if got, ok := ModeForTokenTier("reviewer"); !ok || got != ModeAdvisory || got.CanComment() {
		t.Errorf("reviewer tier = %v,%v; must be ADVISORY and unable to comment on issues", got, ok)
	}
	if got, ok := ModeForTokenTier("merger"); !ok || got != ModeIssuesPRsMerge {
		t.Errorf("merger tier = %v,%v; want ISSUES_PRS_MERGE", got, ok)
	}
	if got, ok := ModeForTokenTier("revoked"); ok || got != ModeAdvisory || got.CanComment() {
		t.Errorf("unknown tier = %v,%v; must fail closed to ADVISORY", got, ok)
	}
}
