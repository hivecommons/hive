package github

// Stage-engine issue facts (ADR-0025, plan PR-F). issueFactsFrom turns the
// enumeration cycle's IssueStageInput into stageengine.Facts, resolving the
// hive's configurable label families (hold, blocked, park, umbrella,
// reporter-trust) on the way; the stage itself is derived by
// stageengine.DeriveIssue. IssueStageFor is the shim the sweep and the
// golden corpus (issue_stage_goldens) call.

import (
	"strings"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// issueFactsFrom builds the engine's Facts from one enumerated issue.
// prStage resolves the covering PR's stage from the shared store (nil =
// unknown), exactly as SyncIssueStageLabels passes it to IssueStageFor.
func issueFactsFrom(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) stageengine.Facts {
	// Refs claims stay actionable in enumeration, but dispatch withholds them:
	// the live claim, not the enumeration bucket, determines coverage (#13747).
	if in.Claim != nil && in.Claim.PRNumber > 0 && !in.Claim.MergedPR && in.Claim.Decision == ClaimDecisionCoveredByOpenPR {
		in.Bucket = workBucketClaimedByPR
	}
	owner, repo, _ := strings.Cut(strings.TrimSpace(in.Repo), "/")
	f := stageengine.Facts{
		Ref:    stageengine.Ref{Owner: owner, Repo: repo, Number: in.Number, Kind: stagelabels.IssueItem},
		Labels: append([]string(nil), in.Labels...),
		Open:   true,
		Author: stageengine.AuthorFacts{Kind: in.AuthorKind},
		Issue: &stageengine.IssueFacts{
			Bucket:         issueBucketFrom(in.Bucket),
			FilteredReason: in.Reason,
		},
	}
	resolveIssueLabelFacts(in, &f)
	if in.Children != nil && in.Children.Total > 0 {
		f.Issue.OpenChildren = in.Children.Open()
	}
	if in.Claim != nil || in.ClaimKind != "" || in.ClaimedBy != "" || in.PRStarted {
		c := &stageengine.ClaimFact{Holder: in.ClaimedBy, Kind: claims.Kind(in.ClaimKind), PRStarted: in.PRStarted}
		if in.Claim != nil {
			c.PRRepo, c.PRNumber = in.Claim.PRRepo, in.Claim.PRNumber
			// A non-closing "Refs #N" claim stages 040 but never "covers".
			c.Partial = in.Claim.Reference && !in.Claim.Closing
		}
		f.Issue.Claim = c
	}
	if in.Bucket == workBucketClaimedByPR && in.Claim != nil && in.Claim.PRNumber > 0 {
		prRepo := in.Claim.PRRepo
		if prRepo == "" {
			prRepo = in.Repo
		}
		prOwner, prName, _ := strings.Cut(strings.TrimSpace(prRepo), "/")
		f.Issue.CoveringPR = &stageengine.Ref{Owner: prOwner, Repo: prName, Number: in.Claim.PRNumber, Kind: stagelabels.PRItem}
		// The Store read is only spent when the deriver can reach 040: a
		// done, held, blocked or parked issue never consults the PR
		// (the goldens pin that the lookup does not happen).
		parked := f.Issue.LikelyDone || f.Issue.HoldLabel != "" || f.Issue.BlockedReason != "" || f.Park.NeedsHuman || f.Park.PossiblyExists || f.Park.DecisionLabel != ""
		if prStage != nil && !parked {
			f.Issue.CoveringPRStage = prStage(prRepo, in.Claim.PRNumber)
		}
	}
	return f
}

// issueBucketFrom maps the enumeration bucket ("" = actionable) to the
// engine's; an unknown bucket is carried as-is.
func issueBucketFrom(bucket string) stageengine.IssueBucket {
	if bucket == "" {
		return stageengine.BucketActionable
	}
	return stageengine.IssueBucket(bucket)
}

// resolveIssueLabelFacts fills the label-family verdicts the engine's
// deriver reads (IssueFacts.HoldLabel, BlockedReason, UmbrellaLabel,
// TriageAwaiting, LikelyDone and the ParkFacts) from the labels and bucket
// in carries, using the hive's configured label vocabulary (hold labels,
// never-claim labels, hard-suppress park labels, the reporter-trust
// awaiting label). It is the one place that vocabulary meets the engine.
func resolveIssueLabelFacts(in IssueStageInput, f *stageengine.Facts) {
	f.Park.Holds = stagelabels.Holds(in.Labels)
	f.Park.NeedsHuman = f.Park.NeedsHuman || issueHeldByNeedsHuman(in)
	// A Parked fact's label parks the issue at 045 only when it is in the
	// hard-suppress park vocabulary, the same test a worn label passes. An
	// operator-renamed decision label is applied by the caller and parks
	// through the issue filter; the engine writes no stage for it (#12329).
	if f.Park.DecisionLabel != "" && hardSuppressIssueLabel([]string{f.Park.DecisionLabel}) == "" {
		f.Park.DecisionLabel = ""
	}
	if f.Park.DecisionLabel == "" {
		f.Park.DecisionLabel = parkedOnPersonLabel(in)
	}
	f.Park.PossiblyExists = f.Park.PossiblyExists || possiblyExistsParked(in)
	is := f.Issue
	is.LikelyDone = issueHasLabel(in.Labels, LikelyDoneLabel)
	is.HoldLabel = issueHoldLabel(in)
	is.NeedsHumanLabel = issueNeedsHumanLabel
	is.Blocker, is.BlockerShort = "", ""
	if b, ok := issueBlockerLookup(in.Repo, in.Number); ok {
		is.Blocker, is.BlockerShort = string(b.Kind), b.Kind.Short()
	}
	is.ApprovedDirectionLabel = ""
	if issueHasLabel(in.Labels, HumanAckLabel) {
		is.ApprovedDirectionLabel = HumanAckLabel
	}
	is.UmbrellaLabel = issueUmbrellaLabel(in.Labels)
	is.BlockedReason = ""
	if hasBlockedLabel(in.Labels) {
		is.BlockedReason = blockedStageReason(in)
	}
	is.TriageAwaiting = ""
	if untriaged, why := issueUntriaged(in); untriaged {
		is.TriageAwaiting = why
	}
	is.TriageAwaitingLabel = ""
	if issueHasLabel(in.Labels, triageAwaitingLabel) {
		is.TriageAwaitingLabel = triageAwaitingLabel
	}
}

// issueBlockerLookup reads the structured blocker recorded on an issue
// (pkg/blocker's shared ledger: an agent's `<!-- hive:blocker -->` marker
// or the prose it inferred from). Tests swap it.
var issueBlockerLookup = func(repo string, number int) (itemblocker.Blocker, bool) {
	return itemblocker.Shared().Blocking(repo, number)
}

// issueInputFrom rebuilds the label/bucket view of an out-of-cycle Facts
// (a FactSink submission enriched with a fresh read) so the same label
// resolution applies to it as to an enumerated issue. An un-park fact is
// resolved without the park labels it lifts (unparkDerivationLabels).
func issueInputFrom(f stageengine.Facts) IssueStageInput {
	in := IssueStageInput{Repo: f.Ref.FullRepo(), Number: f.Ref.Number, Labels: unparkDerivationLabels(f), AuthorKind: f.Author.Kind}
	if f.Issue != nil {
		if f.Issue.Bucket != stageengine.BucketActionable {
			in.Bucket = string(f.Issue.Bucket)
		}
		in.Reason = f.Issue.FilteredReason
	}
	return in
}

// issueDeriver resolves the hive's label vocabulary over the Facts' labels
// and hands the resolved Facts to the engine's deriver (stageengine
// .DeriveIssue). It is the issue Deriver the Client's FactSink and cmd/hive
// use, so an out-of-cycle fact (claim release, un-park, PR close) derives
// exactly as the enumeration sweep would.
type issueDeriver struct{}

// IssueDeriver returns the label-resolving issue Deriver.
func IssueDeriver() stageengine.Deriver { return issueDeriver{} }

// Kind implements stageengine.Deriver.
func (issueDeriver) Kind() stagelabels.Item { return stagelabels.IssueItem }

// Derive implements stageengine.Deriver.
func (issueDeriver) Derive(f stageengine.Facts) stageengine.Decision {
	if f.Ref.Kind == stagelabels.IssueItem && f.Issue != nil {
		issue := *f.Issue
		f.Issue = &issue
		resolveIssueLabelFacts(issueInputFrom(f), &f)
	}
	return stageengine.DeriveIssue(f)
}
