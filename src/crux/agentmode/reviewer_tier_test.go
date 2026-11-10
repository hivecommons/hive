package agentmode

// Tests for #7469: an ADVISORY reviewer must be able to post the verdict it
// just computed.
//
// On a spoke that does not auto-merge, an advisor-tier reviewer's verdict has
// exactly one consumer — the merge-eligibility gate — so the review is
// computed and then discarded, and the humans whose queue it was meant to help
// never see it.

import "testing"

// The reviewer role refines ADVISORY into a tier that may write to pull
// requests. Nothing else moves.
func TestTokenTierForRole_ReviewerGetsCommentCapableTier(t *testing.T) {
	if got := TokenTierForRole(ModeAdvisory, ReviewerRole); got != "reviewer" {
		t.Errorf("ADVISORY reviewer minted %q, want %q — it cannot post its verdict without this", got, "reviewer")
	}
	if got := TokenTierForRole(ModeAdvisory, ""); got != "advisor" {
		t.Errorf("ADVISORY with no role minted %q, want %q", got, "advisor")
	}
	if got := TokenTierForRole(ModeAdvisory, "scanner"); got != "advisor" {
		t.Errorf("ADVISORY scanner minted %q, want %q — only the reviewer role is refined", got, "advisor")
	}
}

// The refinement is scoped to ADVISORY. A reviewer-named agent at a higher mode
// keeps that mode's tier, so the role can never DOWNGRADE a write-capable
// agent to the reviewer tier's read-only contents.
func TestTokenTierForRole_HigherModesAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		mode AgentMode
		want string
	}{
		{ModeIssuesOnly, "newcomer"},
		{ModeIssuesAndPRs, "contributor"},
		{ModeIssuesPRsMerge, "trusted"},
	} {
		if got := TokenTierForRole(tc.mode, ReviewerRole); got != tc.want {
			t.Errorf("%v reviewer minted %q, want %q — the role must not override a higher mode", tc.mode, got, tc.want)
		}
		if got := TokenTierForRole(tc.mode, ""); got != tc.want {
			t.Errorf("%v minted %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// The capability predicates are ordinal comparisons against the mode ladder.
// The reviewer tier is expressed as a ROLE refinement precisely so it does not
// perturb them: an ADVISORY reviewer must still be unable to create issues,
// open PRs, push, or merge. The extra permission it gains is on the minted
// token, not on the mode.
func TestTokenTierForRole_ReviewerGainsNoModeCapability(t *testing.T) {
	m := ModeAdvisory
	if m.CanCreateIssues() {
		t.Error("ADVISORY reports CanCreateIssues — the reviewer tier must not open issues")
	}
	if m.CanCreatePRs() || m.CanPush() {
		t.Error("ADVISORY reports CanCreatePRs/CanPush — the reviewer tier must not push")
	}
	if m.CanMerge() {
		t.Error("ADVISORY reports CanMerge — the reviewer's safety asymmetry depends on this staying false")
	}
}

// The sec-check role refines ISSUES_AND_PRS into a tier whose App token adds
// Dependabot alerts read (#17426). It is scoped to that one role and mode:
// every other contributor-tier agent, and a sec-check agent at any other mode,
// keeps the plain mode tier — except ISSUES_PRS_MERGE, which mints the
// sec-check-trusted tier (#17496).
func TestTokenTierForRole_SecCheckGetsAlertsCapableTier(t *testing.T) {
	if got := TokenTierForRole(ModeIssuesAndPRs, SecCheckRole); got != SecCheckTier {
		t.Errorf("ISSUES_AND_PRS sec-check minted %q, want %q", got, SecCheckTier)
	}
	for _, role := range []string{"", "scanner", ReviewerRole} {
		if got := TokenTierForRole(ModeIssuesAndPRs, role); got != "contributor" {
			t.Errorf("ISSUES_AND_PRS %q minted %q, want contributor — only the sec-check role is refined", role, got)
		}
	}
	for _, tc := range []struct {
		mode AgentMode
		want string
	}{
		{ModeAdvisory, "advisor"},
		{ModeIssuesOnly, "newcomer"},
	} {
		if got := TokenTierForRole(tc.mode, SecCheckRole); got != tc.want {
			t.Errorf("%v sec-check minted %q, want %q", tc.mode, got, tc.want)
		}
	}
	if got := TokenTierForRole(ModeIssuesPRsMerge, SecCheckRole); got != SecCheckTrustedTier {
		t.Errorf("ISSUES_PRS_MERGE sec-check minted %q, want %q", got, SecCheckTrustedTier)
	}
	for _, role := range []string{"", "scanner", ReviewerRole} {
		if got := TokenTierForRole(ModeIssuesPRsMerge, role); got != "trusted" {
			t.Errorf("ISSUES_PRS_MERGE %q minted %q, want trusted — only the sec-check role is refined", role, got)
		}
	}
	if mode, ok := ModeForTokenTier(SecCheckTrustedTier); !ok || mode != ModeIssuesPRsMerge {
		t.Errorf("ModeForTokenTier(%q) = %v, %v; want ISSUES_PRS_MERGE, true", SecCheckTrustedTier, mode, ok)
	}
	if mode, ok := ModeForTokenTier(SecCheckTier); !ok || mode != ModeIssuesAndPRs {
		t.Errorf("ModeForTokenTier(%q) = %v, %v; want ISSUES_AND_PRS, true", SecCheckTier, mode, ok)
	}
}
