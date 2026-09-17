package github

import "strconv"

// MergeVerdictState is the governor's answer to "would the merge sweep take
// this PR now?" — the one definition of "ready" the dashboard is allowed to
// paint green (hivecommons/hive#7478).
//
// GitHub's mergeable verdict alone is not it: mergeable_state="unstable"
// means "no conflicts and nothing REQUIRED is failing", which on a repo
// without branch protection includes PRs with red CI and no review. The
// governor's merge-eligible classifier (writeMergeEligible) applies the gates
// the sweep actually enforces — draft, intent verification, CI, hold, review
// approval, conflicts — and this type carries that classification, per PR,
// to the operator's screen with the reason attached.
type MergeVerdictState string

const (
	// MergeVerdictEligible: every gate passed; the PR is in merge-eligible.json
	// and the sweep would merge it now. The only state a pill may paint green.
	MergeVerdictEligible MergeVerdictState = "eligible"
	// MergeVerdictOutstanding: GitHub reports the PR mergeable, but the sweep
	// still wants something — a red check with no required-check set to call
	// it optional, a hold label, a missing review approval, an intent verdict.
	// Amber, with Reason saying what. This is the honest colour for
	// "unstable".
	MergeVerdictOutstanding MergeVerdictState = "outstanding"
	// MergeVerdictBlocked: GitHub itself says no (conflicts, a required review
	// or status missing, draft). Nothing lights; Reason names the state.
	MergeVerdictBlocked MergeVerdictState = "blocked"
	// MergeVerdictUnknown: mergeability has not been fetched (or GitHub is
	// still computing it). Nothing lights, and the tooltip says so rather than
	// guessing either way.
	MergeVerdictUnknown MergeVerdictState = "unknown"
)

// MergeVerdict is one PR's sweep classification with the reason an operator
// reads in the pill tooltip. Reason is display text: for Eligible it names
// what is still outstanding on the GitHub side that the sweep chooses to
// ignore (non-required checks), for the other states it says why.
type MergeVerdict struct {
	State  MergeVerdictState `json:"state"`
	Reason string            `json:"reason,omitempty"`
}

// MergeVerdictKey is the map key the governor records verdicts under and the
// dashboard looks them up by: the PR's Repo exactly as the enumeration
// spelled it (bare or owner/name) plus its number. Both sides read the same
// ActionableResult, so the spelling agrees by construction.
func MergeVerdictKey(pr PullRequest) string {
	return pr.Repo + "#" + strconv.Itoa(pr.Number)
}
