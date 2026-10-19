package automerge

// Stage-engine adapter (ADR-0025, plan PR-B). factsFrom turns one cycle's
// lane observations of a PR into stageengine.Facts; prDeriverAdapter turns
// them back into stageObservations and runs today's resolveStage, so the
// engine's DerivePR reproduces the lane precedence byte-for-byte while the
// derivation body still lives here. No logic moves in this file; PR-F moves
// classifyStage/resolveStage into pkg/stageengine and deletes it.

import (
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// factsFrom builds the engine's Facts from a PR's lane observations (the
// drained, precedence-sorted list ApplyStageLabels hands resolveStage).
// Details the tracker shares across lanes (verdict, human approval, red
// checks, reviews, DCO) are folded once into PRFacts; per-lane evidence
// becomes one LaneFact each, in the list's order. An empty list yields
// Facts with no PR evidence; the adapter abstains on it.
func factsFrom(list []stageObservation) stageengine.Facts {
	if len(list) == 0 {
		return stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.PRItem}}
	}
	first := list[0]
	f := stageengine.Facts{
		Ref:    stageengine.Ref{Owner: first.Owner, Repo: first.Repo, Number: first.Number, Kind: stagelabels.PRItem},
		Labels: append([]string(nil), first.Labels...),
		Open:   true,
		Author: stageengine.AuthorFacts{Login: first.Author, Role: stagelabels.RoleOf(first.Labels)},
		Park:   stageengine.ParkFacts{Holds: stagelabels.Holds(first.Labels)},
		PR: &stageengine.PRFacts{
			HeadSHA: first.HeadSHA,
			BaseRef: first.BaseRef,
		},
	}
	pr := f.PR
	var detail stageDetail
	haveDetail := false
	for _, o := range list {
		o.Detail = normalizeStageDetail(o.Detail)
		if o.Merged {
			f.Merged = true
		}
		if o.Reason == "draft" {
			f.Draft = true
		}
		if pr.HeadSHA == "" {
			pr.HeadSHA = o.HeadSHA
		}
		if f.Author.Login == "" {
			f.Author.Login = o.Author
		}
		if f.Author.Kind == authorkind.Unknown {
			f.Author.Kind = o.AuthorKind
		}
		if o.Lane == stageLaneSelf {
			f.Author.SelfAuthored = true
		}
		if pr.SharedCIIncident == 0 {
			pr.SharedCIIncident = o.SharedCIIncident
		}
		// After StageTracker.drain every lane carries the same shared
		// detail; the first lane that knows the verdict (or any detail at
		// all) is the one folded.
		if !haveDetail || (!detail.VerdictKnown && o.Detail.VerdictKnown) {
			detail, haveDetail = o.Detail, true
		}
		pr.Lanes = append(pr.Lanes, stageengine.LaneFact{
			Lane:          o.Lane,
			Reason:        o.Reason,
			Err:           o.Err,
			Merged:        o.Merged,
			Role:          o.Detail.Role,
			RedChecks:     append([]string(nil), o.Detail.RedChecks...),
			RedChecksHead: o.Detail.RedChecksHead,
		})
	}
	pr.Review = reviewFactsFrom(detail, pr.HeadSHA)
	pr.DCO = detail.DCO
	pr.DCOGate = detail.DCOGate && (detail.DCOGateHead == "" || pr.HeadSHA == "" || detail.DCOGateHead == pr.HeadSHA)
	return f
}

// reviewFactsFrom folds the lane detail's verdict into the one ReviewFacts
// (ADR-0025 Decision §6). The folding is lossless for what classifyStage
// reads: Approved ↔ VerdictApproved, the raw verdict string, the decision
// request flag and a human approval that matches the head.
func reviewFactsFrom(d stageDetail, head string) stageengine.ReviewFacts {
	r := stageengine.ReviewFacts{
		Verdict:           d.Verdict.Verdict,
		VerdictKnown:      d.VerdictKnown,
		HumanReviews:      d.Reviews,
		HumanReviewsKnown: d.ReviewsKnown,
		DecisionRequested: d.VerdictKnown && d.Verdict.DecisionRequested,
	}
	if approver := strings.TrimSpace(d.HumanApprover); approver != "" && strings.TrimSpace(d.HumanApprovalHead) != "" && strings.TrimSpace(d.HumanApprovalHead) == strings.TrimSpace(head) {
		r.HumanApprover = approver
	}
	r.State = verdictStateFrom(d)
	return r
}

func verdictStateFrom(d stageDetail) stagelabels.VerdictState {
	if !d.VerdictKnown {
		return stagelabels.VerdictPending
	}
	switch {
	case d.Verdict.Approved:
		return stagelabels.VerdictApproved
	case !d.Verdict.Configured:
		return stagelabels.VerdictUnavailable
	case verdictBlocks(d):
		return stagelabels.VerdictChangesRequested
	case verdictNeedsHuman(d):
		return stagelabels.VerdictRequiresHuman
	}
	return stagelabels.VerdictPending
}

// verdictFrom is the inverse of reviewFactsFrom for the fields classifyStage
// reads. The text fields the normalizer consults are empty: the detail was
// normalized before folding and normalization is idempotent, so the
// round-trip classifies identically.
func verdictFrom(r stageengine.ReviewFacts) ContributorLaneVerdict {
	return ContributorLaneVerdict{
		Configured:        r.State != stagelabels.VerdictUnavailable,
		Approved:          r.State == stagelabels.VerdictApproved,
		Verdict:           r.Verdict,
		DecisionRequested: r.DecisionRequested,
	}
}

// observationsFrom rebuilds the lane observations resolveStage expects from
// Facts. Shared PR evidence is copied onto every lane's detail, exactly as
// StageTracker.drain does.
func observationsFrom(f stageengine.Facts) []stageObservation {
	if f.PR == nil {
		return nil
	}
	pr := f.PR
	detail := stageDetail{
		Verdict:      verdictFrom(pr.Review),
		VerdictKnown: pr.Review.VerdictKnown,
		Reviews:      pr.Review.HumanReviews,
		ReviewsKnown: pr.Review.HumanReviewsKnown,
		DCO:          pr.DCO,
		DCOGate:      pr.DCOGate,
	}
	if pr.Review.HumanApprover != "" && pr.HeadSHA != "" {
		detail.HumanApprover, detail.HumanApprovalHead = pr.Review.HumanApprover, pr.HeadSHA
	}
	if pr.DCOGate {
		detail.DCOGateHead = pr.HeadSHA
	}
	out := make([]stageObservation, 0, len(pr.Lanes))
	for _, l := range pr.Lanes {
		d := detail
		d.Role = l.Role
		d.RedChecks = append([]string(nil), l.RedChecks...)
		d.RedChecksHead = l.RedChecksHead
		out = append(out, stageObservation{
			Lane:             l.Lane,
			Display:          f.Ref.FullRepo(),
			Owner:            f.Ref.Owner,
			Repo:             f.Ref.Repo,
			Number:           f.Ref.Number,
			HeadSHA:          pr.HeadSHA,
			Labels:           append([]string(nil), f.Labels...),
			Reason:           l.Reason,
			Merged:           l.Merged,
			Err:              l.Err,
			Detail:           d,
			Author:           f.Author.Login,
			AuthorKind:       f.Author.Kind,
			BaseRef:          pr.BaseRef,
			SharedCIIncident: pr.SharedCIIncident,
		})
	}
	return out
}

// prDeriverAdapter implements stageengine.Deriver over resolveStage.
type prDeriverAdapter struct{}

// PRDeriver returns the automerge-backed stageengine.Deriver for pull
// requests; cmd/hive registers it with stageengine.SetDerivers.
func PRDeriver() stageengine.Deriver { return prDeriverAdapter{} }

// Kind implements stageengine.Deriver.
func (prDeriverAdapter) Kind() stagelabels.Item { return stagelabels.PRItem }

// Derive implements stageengine.Deriver: the lane observations are rebuilt
// from Facts and resolveStage picks the owning opinion. A verdict with no
// rank (no lane could classify the PR) is an Abstain, as ApplyStageLabels
// skips it today.
func (prDeriverAdapter) Derive(f stageengine.Facts) stageengine.Decision {
	if f.Ref.Kind != stagelabels.PRItem {
		return stageengine.Decision{Abstain: true, Reason: "stage facts: not a pull request"}
	}
	list := observationsFrom(f)
	if len(list) == 0 {
		if d, ok := parkedPRDecision(f); ok {
			return d
		}
		return stageengine.Decision{Abstain: true, Reason: "stage facts: no lane observed the PR"}
	}
	verdict, role, obs := resolveStage(list)
	return decisionFrom(verdict, role, obs, f)
}

// decisionFrom renders resolveStage's answer as a Decision. The recorded
// reason is the verdict's own when it set one, else the owning lane's.
func decisionFrom(v stageVerdict, role stagelabels.Role, obs stageObservation, f stageengine.Facts) stageengine.Decision {
	if v.Rank == stageRankNone {
		return stageengine.Decision{Abstain: true, Reason: obs.Reason}
	}
	d := stageengine.Decision{
		Stage:    v.Stage,
		Role:     role,
		Kind:     f.Author.Kind,
		Terminal: v.Terminal,
		Reason:   v.Reason,
	}
	if f.PR != nil {
		d.DCO = f.PR.DCO
	}
	if d.Reason == "" {
		d.Reason = obs.Reason
	}
	if v.Human && !v.Terminal {
		d.Human = &stagelabels.Route{Stage: v.Stage}
	}
	return d
}

// parkedPRDecision is the opinion for a PR no lane has observed but whose
// Facts say a person parked it (an out-of-cycle Parked fact from the issue
// request relay, or a needs-decision label found on a fresh read): it sits
// at 085 on the human route with whatever role it already wears until the
// next sweep's lanes speak. An open PR with no park and no lanes stays an
// Abstain.
func parkedPRDecision(f stageengine.Facts) (stageengine.Decision, bool) {
	if !f.Open || f.Park.ApprovedDirection {
		return stageengine.Decision{}, false
	}
	label := strings.TrimSpace(f.Park.DecisionLabel)
	if label == "" && f.Park.NeedsHuman {
		label = stageengine.NeedsHumanLabel
	}
	if label == "" {
		return stageengine.Decision{}, false
	}
	return stageengine.Decision{
		Stage:  stagelabels.NeedsDecision,
		Role:   stagelabels.RoleOf(f.Labels),
		Kind:   f.Author.Kind,
		Reason: "parked on a person: " + label,
		Human:  &stagelabels.Route{Stage: stagelabels.NeedsDecision},
	}, true
}
