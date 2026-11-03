package dashboard

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// "Let hive decide" on a PR at stage/090 Ready for human merge
// (hivecommons/hive#15078).
//
// The lanes already decided this one: nothing about the PR is wrong, the
// hive simply will not vouch for its author (not a collaborator, no
// trusted / merger tier, a fork from outside the organization, …). Running
// the owner-delegate policy would only record an approval the merge gate
// then ignores, and the operator would learn that thirty minutes later
// through the stalled-decision path. So the hive answers at once, in the
// same no-effect shape the SPA already renders: why it cannot merge this
// itself, and the buttons that do — Approve and let hive merge (lead),
// Merge now as maintainer — plus how to trust the author next time.

// delegatedHumanMergeRule is the rule slug on the answer; it resolves in
// the glossary to the plain words the toast head shows.
const delegatedHumanMergeRule = delegatedecision.GlossaryHumanMerge

// delegatedHumanMergeReason picks the 090 lane reason the row showed, from
// the request's reason / caption or the gathered reasons; "" when none.
func delegatedHumanMergeReason(in delegatedecision.Input, req delegatedDecisionRequest) string {
	candidates := append([]string{req.Reason, req.Caption}, in.Reasons...)
	for _, c := range candidates {
		if c = strings.TrimSpace(c); c != "" && stageengineprstage.HumanMergeReason(c) != "" {
			return c
		}
	}
	return ""
}

// delegatedHumanMergeOptions are the answer's buttons: the two merge
// actions (merger / owner only; "Approve and let hive merge" leads), then send back (hive-authored PRs only:
// a direction has no agent to carry it on a person's PR) and close.
func delegatedHumanMergeOptions(in delegatedecision.Input, number int, mergerRole bool) []StageTrackerDecisionOption {
	ref := "#" + strconv.Itoa(number)
	mergeNow := decisionOption(decisionActionMergeNow, decisionMergeNowLabel, mergerRole, decisionWhyMergerRole)
	mergeNow.Consequence = "merges " + ref + " now via hive's identity, attributed to you"
	queue := decisionOption(decisionActionQueueMerge, decisionQueueMergeLabel, mergerRole, decisionWhyMergerRole)
	queue.Consequence = "records your approval; the hive's merge sweep merges " + ref + " on its next tick once CI is green and the head has soaked"
	direction := decisionOption(decisionActionDirection, "Send back for a new commit", in.HiveAuthored, "The hive did not open this PR, so it cannot carry a direction to its author; leave a changes-requested review on GitHub")
	closeSup := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	return []StageTrackerDecisionOption{queue, mergeNow, direction, closeSup}
}

// decisionWhyMergerRole is the plain reason a merge button is greyed.
const decisionWhyMergerRole = "merger role required"

// delegatedHumanMergeOutcome answers "Let hive decide" on a PR wearing the
// 090 label without running the policy. ok=false for anything else.
func (s *Server) delegatedHumanMergeOutcome(repoFull string, number int, itemType, user string, mergerRole bool, in delegatedecision.Input, req delegatedDecisionRequest) (delegatedOutcome, bool) {
	if in.Kind != delegatedecision.KindPR || !slices.Contains(in.Labels, stagelabels.ReadyForHumanMerge.Label()) {
		return delegatedOutcome{}, false
	}
	reason := delegatedHumanMergeReason(in, req)
	base, _ := stagelabels.HumanApprovedReason(reason)
	base = strings.TrimSpace(base)
	code := stageengineprstage.HumanMergeReason(reason)
	if code == "" {
		code = delegatedHumanMergeRule
	}
	why := stageTrackerUntrustedAuthorWhy(base, in.Author)
	if fix := stageTrackerUntrustedAuthorFix(base, in.Author); why != "" && fix != "" {
		why += ". " + strings.TrimSuffix(fix, ".")
	}
	if why == "" {
		why = decisionPlainReason(base)
		if base == "" || why == base {
			why = "the hive's merge rules leave this PR to a person"
		}
	}
	who := "its author"
	if a := strings.TrimSpace(in.Author); a != "" {
		who = "author @" + a
	}
	_, repoName, _ := strings.Cut(repoFull, "/")
	if repoName == "" {
		repoName = repoFull
	}
	options := delegatedHumanMergeOptions(in, number, mergerRole)
	d := delegatedecision.Decision{
		Verdict:   delegatedecision.VerdictRecommendOnly,
		Rule:      delegatedHumanMergeRule,
		Text:      "I can't merge this: " + who + " is untrusted on " + repoName + ".",
		Rationale: why,
		KeepsPark: true,
	}
	res := RowActionResult{
		Action:  rowActionNoEffect,
		Target:  rowActionTarget(repoFull, rowActionTargetPR, number),
		Rule:    delegatedHumanMergeRule,
		Why:     delegatedecision.GlossaryHumanMerge,
		Options: options,
		Outcome: "I can't merge this: " + who + " is untrusted on " + repoName + " — " + why + ".",
		Next:    "A maintainer merges it. Options: " + strings.Join(optionLabels(options), " · ") + ". To let the hive merge their PRs next time, promote the author to trusted under 👥 Contributors.",
	}
	s.audit.LogRecord(user, auditRepoItemDecisionDelegated, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType,
		"verdict", string(d.Verdict), "rule", d.Rule, "actions", "", "noop", "human-merge:"+code, "policy", delegatedecision.PolicyName, "decision", d.Text), "", repoFull, number)
	return delegatedOutcome{Decision: &d, Notes: []string{"ready for human merge (" + code + "); nothing written"}, Result: &res}, true
}

// "Let hive decide" on an untrusted author's 090 PR takes the decision
// (hivecommons/hive#15173) instead of answering that it cannot merge: when
// nothing is wrong with the PR the owner's delegate approves it — the same
// path as "Approve and let hive merge", so the merge sweep merges the PR under
// its own number once the head has soaked — and only a red, conflicting or
// unreviewed head falls back to adoption in supersede mode.
const (
	delegatedHumanMergeApproveRule     = "human-merge-approve"
	delegatedHumanMergeAdoptCIRedRule  = "human-merge-adopt-ci-red"
	delegatedHumanMergeAdoptRebaseRule = "human-merge-adopt-conflicting"
	delegatedHumanMergeAdoptReviewRule = "human-merge-adopt-review"
	delegatedHumanMergeAdoptDCORule    = "human-merge-adopt-dco"
)

// delegatedHumanMergeDecidedPath is the chosen path and its reason for a
// rule this file decides, as the tracker's ✓ hover reads it after
// "hive decided: "; "" for any other rule.
func delegatedHumanMergeDecidedPath(rule string) string {
	switch rule {
	case delegatedHumanMergeApproveRule:
		return "approve · review passed, CI green"
	case delegatedHumanMergeAdoptCIRedRule:
		return "adopt · CI red"
	case delegatedHumanMergeAdoptRebaseRule:
		return "adopt · conflicting, needs rebase"
	case delegatedHumanMergeAdoptReviewRule:
		return "adopt · review not passed"
	case delegatedHumanMergeAdoptDCORule:
		return "adopt · DCO sign-off missing"
	case delegatedecision.RuleReturnToAuthor:
		return "return to author · a person pushed this PR; the hive asked them, not adopted it"
	}
	return ""
}

// delegatedHumanMergeReviewPassed reports the review swarm settled the
// head without blocking findings: no reviewing, changes-requested or
// requires-human verdict label (review.verdict_labels) is on the PR.
func (s *Server) delegatedHumanMergeReviewPassed(in delegatedecision.Input) bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return true
	}
	v := s.deps.Config.Review.VerdictLabels
	for _, l := range []string{v.Reviewing, v.ChangesRequested, v.RequiresHuman} {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if slices.ContainsFunc(in.Labels, func(have string) bool { return strings.EqualFold(strings.TrimSpace(have), l) }) {
			return false
		}
	}
	return true
}

// delegatedHumanMergeDecision decides a 090 PR held only for its author's
// trust. ok=false leaves the answer to delegatedHumanMergeOutcome: another
// 090 reason, a hive-authored PR, a sentinel alert (a person's call), the
// delegating user's own PR, an approval the user's role cannot give, or an
// adoption this hive has turned off.
func (s *Server) delegatedHumanMergeDecision(in delegatedecision.Input, req delegatedDecisionRequest, user string, mergerRole bool) (delegatedecision.Decision, []string, bool) {
	if in.Kind != delegatedecision.KindPR || !slices.Contains(in.Labels, stagelabels.ReadyForHumanMerge.Label()) {
		return delegatedecision.Decision{}, nil, false
	}
	if stageengineprstage.HumanMergeReason(delegatedHumanMergeReason(in, req)) != stageengineprstage.HumanMergeReasonUntrustedAuthor {
		return delegatedecision.Decision{}, nil, false
	}
	if in.HiveAuthored || in.SentinelAlert || strings.EqualFold(strings.TrimSpace(in.Author), strings.TrimSpace(user)) {
		return delegatedecision.Decision{}, nil, false
	}
	ref := "#" + strconv.Itoa(in.Number)
	adopt := func(rule, why string) (delegatedecision.Decision, []string, bool) {
		if in.PersonAuthored() {
			// A person pushed this PR: it is theirs to fix, never the
			// hive's to adopt (hivecommons/hive#15202).
			if d, ok := delegatedecision.ReturnToAuthor(in); ok {
				return d, []string{"hive decided: return to author · " + why}, true
			}
			return delegatedecision.Decision{}, nil, false
		}
		if !s.stageAdoptionEnabled() {
			return delegatedecision.Decision{}, nil, false
		}
		text := "supersede now: adopt " + ref + " in supersede mode, preserving the author's commits and sign-off — " + why
		return delegatedecision.Decision{
			Verdict:   delegatedecision.VerdictAdoptSupersede,
			Rule:      rule,
			Text:      text,
			Rationale: "the PR waits on a person only because its author is untrusted, and " + why + ", so the hive adopts it to finish the work (hivecommons/hive#15173)",
			Actions:   []delegatedecision.Action{{Kind: delegatedecision.ActionCommentDecision, Text: text}, {Kind: delegatedecision.ActionAdoptSupersede}},
		}, []string{"hive decided: " + delegatedHumanMergeDecidedPath(rule)}, true
	}
	switch {
	case in.Conflicting:
		return adopt(delegatedHumanMergeAdoptRebaseRule, "the branch conflicts with the base and needs a rebase")
	case in.DCOMissing:
		return adopt(delegatedHumanMergeAdoptDCORule, "the DCO sign-off is missing from its commits")
	case !in.CIGreen:
		return adopt(delegatedHumanMergeAdoptCIRedRule, "CI is not green on its head")
	case !s.delegatedHumanMergeReviewPassed(in):
		return adopt(delegatedHumanMergeAdoptReviewRule, "the review has not passed on its head")
	case !mergerRole:
		return delegatedecision.Decision{}, nil, false
	}
	text := "approve " + ref + ": review passed, CI green and mergeable — the hive's merge sweep merges it under its own number once the head has soaked"
	return delegatedecision.Decision{
		Verdict:   delegatedecision.VerdictApprove,
		Rule:      delegatedHumanMergeApproveRule,
		Text:      text,
		Rationale: "nothing about the PR is wrong; only its author's trust kept it from merging, and the owner delegated that call (hivecommons/hive#15173)",
		Actions:   []delegatedecision.Action{{Kind: delegatedecision.ActionCommentDecision, Text: text}, {Kind: delegatedecision.ActionApproveReview}},
	}, []string{"hive decided: " + delegatedHumanMergeDecidedPath(delegatedHumanMergeApproveRule)}, true
}

// admitDelegatedHumanMergeHead writes the delegating person's approval as
// the trust override for the PR's current head before the approval lands —
// what "Approve and let hive merge" writes (hivecommons/hive#15136) — so the
// adoption engine cannot take the PR over before the sweep merges it.
func (s *Server) admitDelegatedHumanMergeHead(ctx context.Context, repoFull string, number int, user string) string {
	state, err := s.deps.GHClient.GetPRState(ctx, repoFull, number)
	if err != nil {
		return "head not admitted (" + err.Error() + "); the lane derives the admission from the recorded approval"
	}
	if by := s.admitPullRequestHead(repoFull, number, state.HeadSHA, user); by != "" {
		return "head " + state.HeadSHA + " admitted (" + by + "): merged as-is, not adopted"
	}
	return "no stage record to admit the head on; the lane derives the admission from the recorded approval"
}
