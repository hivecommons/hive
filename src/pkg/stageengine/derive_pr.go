package stageengine

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// FactsFromPRObservations builds the engine's Facts from a PR's lane observations (the
// drained, precedence-sorted list ApplyStageLabels hands ResolveStage).
// Details the tracker shares across lanes (verdict, human approval, red
// checks, reviews, DCO) are folded once into PRFacts; per-lane evidence
// becomes one LaneFact each, in the list's order. An empty list yields
// Facts with no PR evidence; the adapter abstains on it. What the stage
// pass learns after ResolveStage (author kind, route evidence, DCO walk,
// adoption state) is added by ApplyStageLabels as PRFacts.Pass.
func FactsFromPRObservations(list []stageObservation) Facts {
	if len(list) == 0 {
		return Facts{Ref: Ref{Kind: stagelabels.PRItem}}
	}
	first := list[0]
	f := Facts{
		Ref:    Ref{Owner: first.Owner, Repo: first.Repo, Number: first.Number, Kind: stagelabels.PRItem},
		Labels: append([]string(nil), first.Labels...),
		Open:   true,
		Author: AuthorFacts{Login: first.Author, Role: stagelabels.RoleOf(first.Labels)},
		Park:   ParkFacts{Holds: stagelabels.Holds(first.Labels)},
		PR: &PRFacts{
			HeadSHA: first.HeadSHA,
			BaseRef: first.BaseRef,
		},
	}
	pr := f.PR
	var detail stageDetail
	haveDetail := false
	for _, o := range list {
		o.Detail = NormalizeStageDetail(o.Detail)
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
		if len(pr.BaseRedChecks) == 0 {
			pr.BaseRedChecks = append([]string(nil), o.BaseRedChecks...)
		}
		if len(pr.ForkGuardChecks) == 0 {
			pr.ForkGuardChecks = append([]string(nil), o.ForkGuardChecks...)
		}
		// After StageTracker.drain every lane carries the same shared
		// detail; the first lane that knows the verdict (or any detail at
		// all) is the one folded.
		if !haveDetail || (!detail.VerdictKnown && o.Detail.VerdictKnown) {
			detail, haveDetail = o.Detail, true
		}
		pr.Lanes = append(pr.Lanes, LaneFact{
			Lane:          o.Lane,
			Reason:        o.Reason,
			Err:           o.Err,
			Merged:        o.Merged,
			Role:          o.Detail.Role,
			RedChecks:     append([]string(nil), o.Detail.RedChecks...),
			RedChecksHead: o.Detail.RedChecksHead,
		})
	}
	pr.Review = ReviewFactsFrom(detail, pr.HeadSHA)
	pr.DCO = detail.DCO
	pr.DCOGate = detail.DCOGate && (detail.DCOGateHead == "" || pr.HeadSHA == "" || detail.DCOGateHead == pr.HeadSHA)
	return f
}

// ReviewFactsFrom folds the lane detail's verdict into the one ReviewFacts
// (ADR-0025 Decision §6). The folding is lossless for what ClassifyStage
// reads: Approved ↔ VerdictApproved, the raw verdict string, the decision
// request flag and a human approval that matches the head.
func ReviewFactsFrom(d stageDetail, head string) ReviewFacts {
	d = NormalizeStageDetail(d)
	r := ReviewFacts{
		Verdict:           d.Verdict.Verdict,
		VerdictKnown:      d.VerdictKnown,
		HumanReviews:      d.Reviews,
		HumanReviewsKnown: d.ReviewsKnown,
		DecisionRequested: d.VerdictKnown && d.Verdict.DecisionRequested,
	}
	if approver := strings.TrimSpace(d.HumanApprover); approver != "" && strings.TrimSpace(d.HumanApprovalHead) != "" && strings.TrimSpace(d.HumanApprovalHead) == strings.TrimSpace(head) {
		r.HumanApprover = approver
	}
	if requester := strings.TrimSpace(d.HumanChangeRequester); requester != "" && strings.TrimSpace(d.HumanChangeRequestHead) != "" && strings.TrimSpace(d.HumanChangeRequestHead) == strings.TrimSpace(head) {
		r.HumanChangeRequester = requester
	}
	if v := d.Verdict; d.VerdictKnown && v.DecisionAnswered && strings.TrimSpace(v.DecisionAnsweredHead) == strings.TrimSpace(head) {
		r.DecisionAnswered, r.DecisionDirective = true, v.DecisionDirective
	}
	r.State = VerdictStateFrom(d)
	return r
}

func VerdictStateFrom(d stageDetail) stagelabels.VerdictState {
	if !d.VerdictKnown {
		return stagelabels.VerdictPending
	}
	switch {
	case d.Verdict.Approved:
		return stagelabels.VerdictApproved
	case !d.Verdict.Configured:
		return stagelabels.VerdictUnavailable
	case VerdictBlocks(d):
		return stagelabels.VerdictChangesRequested
	case VerdictNeedsHuman(d):
		return stagelabels.VerdictRequiresHuman
	}
	return stagelabels.VerdictPending
}

// VerdictFromReviewFacts is the inverse of ReviewFactsFrom for the fields ClassifyStage
// reads. The text fields the normalizer consults are empty: the detail was
// normalized before folding and normalization is idempotent, so the
// round-trip classifies identically.
func VerdictFromReviewFacts(r ReviewFacts) ContributorLaneVerdict {
	return ContributorLaneVerdict{
		Configured:        r.State != stagelabels.VerdictUnavailable,
		Approved:          r.State == stagelabels.VerdictApproved,
		Verdict:           r.Verdict,
		DecisionRequested: r.DecisionRequested,
	}
}

// ObservationsFromPRFacts rebuilds the lane observations ResolveStage expects from
// Facts. Shared PR evidence is copied onto every lane's detail, exactly as
// StageTracker.drain does.
func ObservationsFromPRFacts(f Facts) []stageObservation {
	if f.PR == nil {
		return nil
	}
	pr := f.PR
	detail := stageDetail{
		Verdict:      VerdictFromReviewFacts(pr.Review),
		VerdictKnown: pr.Review.VerdictKnown,
		Reviews:      pr.Review.HumanReviews,
		ReviewsKnown: pr.Review.HumanReviewsKnown,
		DCO:          pr.DCO,
		DCOGate:      pr.DCOGate,
	}
	if pr.Review.HumanApprover != "" && pr.HeadSHA != "" {
		detail.HumanApprover, detail.HumanApprovalHead = pr.Review.HumanApprover, pr.HeadSHA
	}
	if pr.Review.HumanChangeRequester != "" && pr.HeadSHA != "" {
		detail.HumanChangeRequester, detail.HumanChangeRequestHead = pr.Review.HumanChangeRequester, pr.HeadSHA
	}
	if pr.Review.DecisionAnswered && pr.HeadSHA != "" {
		by := pr.Review.HumanChangeRequester
		if pr.Review.DecisionDirective == prstage.DecisionDirectiveApprove {
			by = pr.Review.HumanApprover
		}
		detail.Verdict = detail.Verdict.WithDecisionAnswer(by, pr.HeadSHA, pr.Review.DecisionDirective)
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
			BaseRedChecks:    append([]string(nil), pr.BaseRedChecks...),
			ForkGuardChecks:  append([]string(nil), pr.ForkGuardChecks...),
		})
	}
	return out
}

func derivePR(f Facts) Decision {
	if f.Ref.Kind != stagelabels.PRItem {
		return Decision{Abstain: true, Reason: "stage facts: not a pull request"}
	}
	list := ObservationsFromPRFacts(f)
	if len(list) == 0 {
		if d, ok := adoptedPRDecision(f); ok {
			return d
		}
		if d, ok := parkedPRDecision(f); ok {
			return d
		}
		return Decision{Abstain: true, Reason: "stage facts: no lane observed the PR"}
	}
	verdict, role, obs := ResolveStage(list)
	if f.PR.Pass != nil && verdict.Rank != stageRankNone {
		return passDecision(verdict, role, obs, f)
	}
	return decisionFrom(verdict, role, obs, f)
}

// passDecision applies the pure post-steps ApplyStageLabels runs on the
// resolved opinion, in its order, from the PRPassFacts the pass recorded:
// the 07x author-kind split, the normalized-fixing guard, the human route
// (stagelabels.RouteHuman over the gathered evidence), the DCO override and
// the 074 adoption yield / stalled-adoption escalation. The forge reads and
// store state behind them are inputs here, never re-done, so the Decision
// is the stage the legacy pass wrote for the same Facts.
func passDecision(verdict stageVerdict, role stagelabels.Role, obs stageObservation, f Facts) Decision {
	pass := f.PR.Pass
	obs.AuthorKind = pass.AuthorKind
	if verdict.Stage == stagelabels.NeedsAuthor || verdict.Stage == stagelabels.ReturnedToContributor {
		verdict.Stage = FixingStage(obs)
	}
	obs.LaneReason = obs.Reason
	if verdict.Reason != "" {
		obs.Reason = verdict.Reason
	}
	if pass.Prior != nil {
		prev := stagelabels.Record{Stage: pass.Prior.Stage, Reason: pass.Prior.Reason, ActivityHead: pass.Prior.ActivityHead}
		if replacement, ok := PreserveNormalizedFixingStage(prev, true, obs, verdict); ok {
			verdict = replacement
			obs.Reason = replacement.Reason
		}
		if replacement, ok := PreserveStageOnStaleDecisionAnswer(prev, true, obs, verdict, pass.FixerExcluded); ok {
			verdict = replacement
			obs.Reason = replacement.Reason
		}
	}
	var human *stagelabels.Route
	if verdict.Human && !verdict.Terminal && verdict.Reason != prstage.ReasonSupersessionPending {
		if pass.Route != nil {
			route := stagelabels.RouteHuman(*pass.Route)
			verdict.Stage = route.Stage
			if route.LaneReason != "" {
				obs.Reason = route.LaneReason
			}
			human = &route
		} else {
			human = &stagelabels.Route{Stage: verdict.Stage}
		}
	}
	dco := f.PR.DCO
	if !verdict.Terminal && verdict.Stage != stagelabels.None && pass.DCOKnown && verdict.Reason != prstage.ReasonSupersessionPending {
		dco = pass.DCO
		if pass.DCO == stagelabels.DCONo && pass.DCORequired {
			verdict.Stage = FixingStage(obs)
			verdict.Rank = stageRankOwned
			obs.Reason = stagelabels.LaneReasonDCOMissing
		} else if pass.SquashUnsafeHive > 0 && !pass.AuthorIsApp && pass.MergeCommit != stagelabels.MergeCommitWanted {
			verdict.Stage = FixingStage(obs)
			verdict.Rank = stageRankOwned
			obs.Reason = stagelabels.LaneReasonHiveSignoffAttribution
		}
	}
	if !verdict.Terminal && pass.AdoptionLive && verdict.Stage.YieldsToAdoptionFor(pass.Route) {
		verdict.Stage = stagelabels.AdoptedByHive
		// 085 yielded to the adoption: no decision is owed on this PR.
		human = nil
		if pass.EscalationReason != "" {
			verdict.Stage = stagelabels.NeedsDecision
			verdict.Rank = stageRankOwned
			obs.Reason = pass.EscalationReason
		}
	}
	if !verdict.Terminal && pass.AdoptionRefusedReason != "" && verdict.Stage == FixingStage(obs) {
		obs.Reason = pass.AdoptionRefusedReason
	}
	if !verdict.Terminal && pass.RebaseFailedTwice && verdict.Stage == FixingStage(obs) {
		verdict.Stage = stagelabels.NeedsDecision
		verdict.Rank = stageRankOwned
		obs.Reason = stagelabels.LaneReasonRebaseFailedTwice
	}
	if !verdict.Terminal && pass.FixerExhausted > 0 && verdict.Stage == FixingStage(obs) {
		verdict.Stage = stagelabels.NeedsDecision
		verdict.Rank = stageRankOwned
		obs.Reason = stagelabels.FixerExhaustedReason(pass.FixerExhausted)
	}
	d := Decision{
		Stage:      verdict.Stage,
		Role:       role,
		Kind:       pass.AuthorKind,
		KindLabels: pass.KindLabels,
		Terminal:   verdict.Terminal,
		Reason:     obs.Reason,
		DCO:        dco,
	}
	if !verdict.Terminal && pass.DCOKnown {
		d.MergeCommit = pass.MergeCommit
	}
	if verdict.Terminal {
		// The pass writes a zero Desired on a terminal PR: every pipeline
		// label (stage, role, stalled) comes off; the author/<kind> label
		// stays (Kind unknown leaves it alone) as a fact about the PR.
		d.Stage, d.Role, d.Kind, d.KindLabels = stagelabels.None, stagelabels.RoleNone, authorkind.Unknown, authorkind.Names{}
	} else {
		d.Human = human
		d.Stalled = pass.Stalled
	}
	return d
}

// decisionFrom renders ResolveStage's answer as a Decision. The recorded
// reason is the verdict's own when it set one, else the owning lane's.
func decisionFrom(v stageVerdict, role stagelabels.Role, obs stageObservation, f Facts) Decision {
	if v.Rank == stageRankNone {
		return Decision{Abstain: true, Reason: obs.Reason}
	}
	d := Decision{
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
// adoptedPRDecision is the stage of an open PR a fact reports as adopted by
// the hive (Facts.Adoption live at the PR's head) when no lane observed
// it: 074 adopted-by-hive, ahead of the 085 park the labels may still
// carry — adoption is who owns the PR, and the owner answers the question
// on the superseding PR. A hold label or sentinel alert keeps the park.
func adoptedPRDecision(f Facts) (Decision, bool) {
	a := f.Adoption
	if !f.Open || !a.Adopted || !a.Live || a.Yielded {
		return Decision{}, false
	}
	if f.PR != nil && a.Head != "" && f.PR.HeadSHA != "" && a.Head != f.PR.HeadSHA {
		return Decision{}, false
	}
	if len(f.Park.Holds) > 0 || f.Park.Sentinel || f.Park.NeedsHuman {
		// A hold, sentinel or `needs-human` label is an explicit park
		// (hivecommons/hive#14471); only the lane's own 085 yields.
		return Decision{}, false
	}
	reason := "adopted by the hive"
	if a.SupersededBy > 0 {
		reason += ": superseded by #" + strconv.Itoa(a.SupersededBy)
	}
	return Decision{
		Stage:  stagelabels.AdoptedByHive,
		Role:   stagelabels.RoleOf(f.Labels),
		Kind:   f.Author.Kind,
		Reason: reason,
	}, true
}

func parkedPRDecision(f Facts) (Decision, bool) {
	if !f.Open || f.Park.ApprovedDirection {
		return Decision{}, false
	}
	label := strings.TrimSpace(f.Park.DecisionLabel)
	if label == "" && f.Park.NeedsHuman {
		label = NeedsHumanLabel
	}
	if label == "" {
		return Decision{}, false
	}
	return Decision{
		Stage:  stagelabels.NeedsDecision,
		Role:   stagelabels.RoleOf(f.Labels),
		Kind:   f.Author.Kind,
		Reason: "parked on a person: " + label,
		Human:  &stagelabels.Route{Stage: stagelabels.NeedsDecision},
	}, true
}
