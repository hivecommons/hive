package dashboard

import (
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
// itself, and the buttons that do — Approve & merge as maintainer, Approve
// — let hive merge — plus how to trust the author next time.

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
// actions (merger / owner only), then send back (hive-authored PRs only:
// a direction has no agent to carry it on a person's PR) and close.
func delegatedHumanMergeOptions(in delegatedecision.Input, number int, mergerRole bool) []StageTrackerDecisionOption {
	ref := "#" + strconv.Itoa(number)
	mergeNow := decisionOption(decisionActionMergeNow, "Approve & merge as maintainer", mergerRole, decisionWhyMergerRole)
	mergeNow.Consequence = "merges " + ref + " now via hive's identity, attributed to you"
	queue := decisionOption(decisionActionQueueMerge, "Approve — let hive merge", mergerRole, decisionWhyMergerRole)
	queue.Consequence = "records your approval; the hive's merge sweep merges " + ref + " on its next pass once CI is green and the head has soaked"
	direction := decisionOption(decisionActionDirection, "Send back for a new commit", in.HiveAuthored, "The hive did not open this PR, so it cannot carry a direction to its author; leave a changes-requested review on GitHub")
	closeSup := decisionOption(decisionActionCloseSuperseded, "Close as superseded", true, "")
	return []StageTrackerDecisionOption{mergeNow, queue, direction, closeSup}
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
