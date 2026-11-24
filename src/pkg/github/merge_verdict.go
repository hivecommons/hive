package github

import (
	"strconv"
)

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
	// "unstable". Also the state of a HandMerge verdict: every per-PR gate
	// passed, but the repo's auto-merge switch is off, so no sweep will ever
	// act and a person merges it.
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
	Tier   string            `json:"tier,omitempty"`
	// HandMerge marks a PR that passes every per-PR gate but sits in a repo
	// whose auto-merge switch is off (config.RepoAutoMergeEnabled: an owner
	// override, or a hive below ACMM L6). No Hive sweep merges there, so the
	// verdict is outstanding rather than eligible and only a person can land
	// it — the dashboard's your-turn decision reads this flag. It also marks
	// a PR that passes every per-PR gate in a repo whose auto-merge is on
	// but whose merge relay the contributor lane refuses for a reason no Hive
	// lane will clear (a trust or admission reason, or the lane switched off
	// or scoped to other repos): no Hive lane merges it either. It changes no
	// merge bucket; only the dashboard reads it.
	HandMerge bool `json:"hand_merge,omitempty"`
	// ReviewChangesRequested marks a verdict for a PR whose current head the
	// hive's review returned for changes (a changes_requested verdict that
	// does not also hold the head for a person), whichever gate stopped it
	// first, as opposed to a head the review has not produced a verdict for
	// yet. Such a verdict never becomes a hand merge in a repo whose
	// auto-merge is off: the review asked for a new head. A requires_human or
	// reject head is not marked; the review's own hold brings it to a person.
	// It changes no merge bucket and no reason; the hand-merge marking and the
	// dashboard's waiting-on-the-author wording read it.
	ReviewChangesRequested bool `json:"review_changes_requested,omitempty"`
}

// MergeVerdictKey is the map key the governor records verdicts under and the
// dashboard looks them up by: the PR's Repo exactly as the enumeration
// spelled it (bare or owner/name) plus its number. Both sides read the same
// ActionableResult, so the spelling agrees by construction.
func MergeVerdictKey(pr PullRequest) string {
	return pr.Repo + "#" + strconv.Itoa(pr.Number)
}
