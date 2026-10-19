package github

// Stage-engine adapter (ADR-0025, plan PR-B). issueFactsFrom turns the
// enumeration cycle's IssueStageInput into stageengine.Facts;
// issueDeriverAdapter turns Facts back into an IssueStageInput and runs
// today's IssueStageFor, so the engine's DeriveIssue reproduces the bucket
// mapping exactly while the derivation body still lives here. No logic
// moves in this file; PR-F moves IssueStageFor into pkg/stageengine.

import (
	"strings"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// issueFactsFrom builds the engine's Facts from one enumerated issue.
// prStage resolves the covering PR's stage from the shared store (nil =
// unknown), exactly as SyncIssueStageLabels passes it to IssueStageFor.
func issueFactsFrom(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) stageengine.Facts {
	owner, repo, _ := strings.Cut(strings.TrimSpace(in.Repo), "/")
	f := stageengine.Facts{
		Ref:    stageengine.Ref{Owner: owner, Repo: repo, Number: in.Number, Kind: stagelabels.IssueItem},
		Labels: append([]string(nil), in.Labels...),
		Open:   true,
		Park: stageengine.ParkFacts{
			Holds:         stagelabels.Holds(in.Labels),
			NeedsHuman:    issueHeldByNeedsHuman(in),
			DecisionLabel: parkedOnPersonLabel(in),
		},
		Issue: &stageengine.IssueFacts{
			Bucket:         issueBucketFrom(in.Bucket),
			FilteredReason: in.Reason,
			LikelyDone:     issueHasLabel(in.Labels, LikelyDoneLabel),
		},
	}
	if in.Claim != nil || in.ClaimKind != "" || in.ClaimedBy != "" {
		c := &stageengine.ClaimFact{Holder: in.ClaimedBy, Kind: claims.Kind(in.ClaimKind)}
		if in.Claim != nil {
			c.PRRepo, c.PRNumber = in.Claim.PRRepo, in.Claim.PRNumber
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
		if prStage != nil {
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

// issueInputFrom is the inverse of issueFactsFrom for the fields
// IssueStageFor reads.
func issueInputFrom(f stageengine.Facts) IssueStageInput {
	in := IssueStageInput{Repo: f.Ref.FullRepo(), Number: f.Ref.Number, Labels: append([]string(nil), f.Labels...)}
	if f.Issue == nil {
		return in
	}
	if f.Issue.Bucket != stageengine.BucketActionable {
		in.Bucket = string(f.Issue.Bucket)
	}
	in.Reason = f.Issue.FilteredReason
	if c := f.Issue.Claim; c != nil {
		in.ClaimKind, in.ClaimedBy = string(c.Kind), c.Holder
		in.Claim = &IssueClaimContext{PRRepo: c.PRRepo, PRNumber: c.PRNumber}
	}
	if pr := f.Issue.CoveringPR; pr != nil && in.Claim == nil {
		in.Claim = &IssueClaimContext{PRRepo: pr.FullRepo(), PRNumber: pr.Number}
	}
	return in
}

// issueDeriverAdapter implements stageengine.Deriver over IssueStageFor.
type issueDeriverAdapter struct{}

// IssueDeriver returns the enumeration-backed stageengine.Deriver for
// issues; cmd/hive registers it with stageengine.SetDerivers.
func IssueDeriver() stageengine.Deriver { return issueDeriverAdapter{} }

// Kind implements stageengine.Deriver.
func (issueDeriverAdapter) Kind() stagelabels.Item { return stagelabels.IssueItem }

// Derive implements stageengine.Deriver. The covering PR's stage comes from
// IssueFacts.CoveringPRStage (the Store read happened when the Facts were
// built), so the deriver stays pure.
func (issueDeriverAdapter) Derive(f stageengine.Facts) stageengine.Decision {
	if f.Ref.Kind != stagelabels.IssueItem {
		return stageengine.Decision{Abstain: true, Reason: "stage facts: not an issue"}
	}
	if f.Issue == nil {
		return stageengine.Decision{Abstain: true, Reason: "stage facts: no enumeration bucket for the issue"}
	}
	// A closed issue is out of the pipeline whatever fact arrived: an
	// out-of-cycle release on an issue that closed meanwhile clears its
	// stage rather than re-staging it 010 (the enumeration never sees it).
	if !f.Open {
		return stageengine.Decision{Stage: stagelabels.None, Terminal: true, Reason: "closed"}
	}
	var prStage func(repo string, number int) stagelabels.Stage
	if pr := f.Issue.CoveringPR; pr != nil {
		want := *pr
		stage := f.Issue.CoveringPRStage
		prStage = func(repo string, number int) stagelabels.Stage {
			if strings.EqualFold(strings.TrimSpace(repo), want.FullRepo()) && number == want.Number {
				return stage
			}
			return stagelabels.None
		}
	}
	stage, _, reason := IssueStageFor(issueInputFrom(f), prStage)
	return stageengine.Decision{
		Stage:    stage,
		Reason:   reason,
		Terminal: stage == stagelabels.None && f.Issue.LikelyDone,
		// The dwell verdict is the sweep's: an out-of-cycle fact carries the
		// label as found (legacy EnforceStage kept it too) and the next
		// sweep re-judges it from the restarted clock.
		Stalled: f.Issue.Stalled && stage != stagelabels.None,
	}
}
