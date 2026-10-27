package stageengine

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Issue derivation (ADR-0025, plan PR-F). deriveIssue is the body that was
// pkg/github.IssueStageFor until the engine took it over: the enumeration
// still resolves the configurable label families (hold, blocked, park,
// umbrella, triage) into IssueFacts, and the stage follows from those
// facts alone. pkg/github's IssueStageFor is a shim over DeriveIssue and
// its golden corpus (issue_stage_goldens) replays through it, so the
// mapping is pinned byte-for-byte on both sides of the move.

const (
	reasonIssueNotIssue   = "stage facts: not an issue"
	reasonIssueNoBucket   = "stage facts: no enumeration bucket for the issue"
	reasonIssueClosed     = "closed"
	reasonIssueLikelyDone = "merged PR awaiting confirmation"
	reasonIssueActionable = "actionable"
	reasonIssueOpenPR     = "open PR covers it"
)

// deriveIssue maps issue Facts to a Decision. Pure: same Facts → same
// Decision. An issue never goes past 040-covered-by-pr: 050+ are PR stages
// (stagelabels.Table, PR: true) and mirroring them onto the issue is what
// left hivecommons/hive#12451 wearing stage/050-review-pending next to its
// own stages.
func deriveIssue(f Facts) Decision {
	if f.Ref.Kind != stagelabels.IssueItem {
		return abstain(reasonIssueNotIssue)
	}
	if f.Issue == nil {
		return abstain(reasonIssueNoBucket)
	}
	// A closed issue is out of the pipeline whatever fact arrived: an
	// out-of-cycle release on an issue that closed meanwhile clears its
	// stage rather than re-staging it 010 (the enumeration never sees it).
	if !f.Open {
		return Decision{Stage: stagelabels.None, Terminal: true, Reason: reasonIssueClosed}
	}
	stage, covered, reason := issueStage(f)
	if f.Issue.OpenChildren > 0 && stage != stagelabels.None {
		// A decomposed parent waits on its children (hivecommons/hive#12857).
		reason += "; waiting on " + strconv.Itoa(f.Issue.OpenChildren) + " open child issue(s)"
	}
	return Decision{
		Stage:    stage,
		Reason:   reason,
		Covered:  covered,
		Progress: issueProgress(f, stage),
		Terminal: stage == stagelabels.None && f.Issue.LikelyDone,
		// The dwell verdict is the sweep's: an out-of-cycle fact carries the
		// label as found and the next sweep re-judges it from the restarted
		// clock.
		Stalled: f.Issue.Stalled && stage != stagelabels.None,
	}
}

// issueStage is the bucket → stage mapping. covered reports whether an open
// PR the lanes have staged at 040 or later covers the issue (health then
// belongs to the PR, whose stage the reason names).
func issueStage(f Facts) (stage stagelabels.Stage, covered bool, reason string) {
	is := f.Issue
	if is.LikelyDone {
		return stagelabels.None, false, reasonIssueLikelyDone
	}
	// A held issue is paused, not parked on a person: it shows as
	// "Open · on hold", and hold outranks needs-human / needs-decision /
	// needs-spec (hivecommons/hive#13012). PR holds keep 085 (RouteHuman).
	if is.HoldLabel != "" {
		return stagelabels.None, false, "held: " + is.HoldLabel
	}
	// A blocked issue waits on an unmerged PR (or another outside blocker):
	// nothing to decide, nothing to claim, so it is parked out of the
	// pipeline like a hold until the sweep lifts the label
	// (hivecommons/hive#13093).
	if is.BlockedReason != "" {
		return stagelabels.None, false, is.BlockedReason
	}
	if f.Park.NeedsHuman {
		return stagelabels.IssueNeedsDecision, false, "held for a person: " + is.NeedsHumanLabel
	}
	// The prior-art gate's park is its own stage, 017, not 045: nothing is
	// being decided by a maintainer — the author is being asked whether
	// the request already exists (hivecommons/hive#13575).
	if f.Park.PossiblyExists {
		return stagelabels.PossiblyExisting, false, "possibly exists: awaiting the author"
	}
	if f.Park.DecisionLabel != "" {
		return stagelabels.IssueNeedsDecision, false, "parked on a person: " + f.Park.DecisionLabel
	}
	// An umbrella issue (governor.claims.never_claim_labels, default
	// `epic`) is the list of work, not the work: a claim or start mark on
	// it is never staged 020/030 — admission refuses new ones, and one that
	// exists (recorded before the gate, or by a human) leaves the epic at
	// its umbrella stage, 010 while open (hivecommons/hive#12999).
	if is.UmbrellaLabel != "" && (is.Bucket == BucketClaimed || is.Bucket == BucketInProgress) {
		stage, reason := actionableIssueStage(is, "umbrella issue ("+is.UmbrellaLabel+"): claim not staged")
		return stage, false, reason
	}
	claim := is.Claim
	prStarted := claim != nil && claim.PRStarted
	switch is.Bucket {
	case BucketActionable, BucketReporterTriage:
		stage, reason := actionableIssueStage(is, reasonIssueActionable)
		return stage, false, reason
	case BucketClaimed:
		if prStarted {
			return stagelabels.PRInProgress, false, "live claim with a PR under way"
		}
		if claim != nil && strings.EqualFold(string(claim.Kind), string(claims.KindAgent)) {
			return stagelabels.Claimed, false, "live agent claim (no PR yet)"
		}
		return stagelabels.Claimed, false, "live issue claim"
	case BucketInProgress:
		if prStarted {
			return stagelabels.PRInProgress, false, "in-progress start mark with a PR under way"
		}
		return stagelabels.Claimed, false, "in-progress start mark (no PR yet)"
	case BucketCoveredByPR:
		if pr := is.CoveringPR; pr != nil && pr.Number > 0 {
			if s := is.CoveringPRStage; s != stagelabels.None && !s.Before(stagelabels.CoveredByPR) {
				return stagelabels.CoveredByPR, true, "open PR #" + strconv.Itoa(pr.Number) + " covers it (PR at " + s.String() + ")"
			}
		}
		return stagelabels.CoveredByPR, false, reasonIssueOpenPR
	}
	return stagelabels.None, false, "outside the pipeline: " + string(is.Bucket)
}

// issueProgress reports whether an issue back at an actionable stage got
// there by a PR landing: close-on-merge labels the issue partially-addressed
// when the merged PR covered only part of it and leaves the remainder at
// 010. Seen from the stage number alone that is 040 → 010, a fall; seen
// from the label it is work that merged, so the Store must not book a
// regression for it.
func issueProgress(f Facts, stage stagelabels.Stage) bool {
	if stage != stagelabels.Actionable && stage != stagelabels.TriageNeeded {
		return false
	}
	return HasLabelFold(f.Labels, stagelabels.PartiallyAddressedLabel)
}

// actionableIssueStage is 010-actionable or, for an untriaged issue, 015.
// Who filed the issue is deliberately not consulted — hive-, agent- and
// trusted-human-filed issues are 010 (owner decision, hivecommons/hive#13287
// follow-up); the enumeration's IssueFacts.TriageAwaiting names why the
// reporter-trust gate holds it when it does.
func actionableIssueStage(is *IssueFacts, reason string) (stagelabels.Stage, string) {
	if is.TriageAwaiting != "" {
		return stagelabels.TriageNeeded, "triage needed: " + is.TriageAwaiting
	}
	return stagelabels.Actionable, reason
}
