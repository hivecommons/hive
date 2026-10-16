package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Gates the dashboard bands and the merge classifier already recognise, fed
// into the your-turn decision: an issue labelled blocked with no recorded
// blocker (hivecommons/hive#12376), and a pull request waiting on a decision
// label, a person's review decision or a hand merge (hivecommons/hive#12378).

const (
	OwnershipReasonBlockedUnexplained = "blocked_unexplained"
	OwnershipReasonReviewDecision     = "review_decision_required"
	OwnershipReasonPRGateLabel        = "pr_gate_label"
	OwnershipReasonHandMerge          = "hand_merge_required"
)

var ownershipGateReasonVerbs = map[string]string{
	OwnershipReasonBlockedUnexplained: OwnershipVerbReview,
	OwnershipReasonReviewDecision:     OwnershipVerbReview,
	OwnershipReasonPRGateLabel:        OwnershipVerbReview,
	OwnershipReasonHandMerge:          OwnershipVerbReview,
}

func init() {
	for code, verb := range ownershipGateReasonVerbs {
		ownershipReasonVerbs[code] = verb
	}
}

// prDiscussingLabel is the human-gate label (prHumanGateLabels) that asks for
// a decision under another name; the bands give it the needs-decision signal.
const prDiscussingLabel = "2-discussing"

// ownershipGateMayDecide reports whether the gate rules may still decide an
// item: the precedence rules left it not your turn and not waiting elsewhere,
// or surfaced it only through the stale safety net, which a named gate
// explains better.
func ownershipGateMayDecide(out Ownership) bool {
	if out.WaitingElsewhere {
		return false
	}
	return !out.YourTurn || out.ReasonCode == OwnershipReasonStaleSafetyNet
}

// decideIssueOwnershipGated is decideIssueOwnership with the blocked rule.
func decideIssueOwnershipGated(in issueOwnershipInput, env ownershipEnv) Ownership {
	out := decideIssueOwnership(in, env)
	if !ownershipGateMayDecide(out) {
		return out
	}
	if gated, ok := blockedIssueOwnership(in, env); ok {
		return gated
	}
	return out
}

// blockedIssueOwnership splits the blocked label (a gate only a human lifts)
// by what it waits on. With an open GitHub blocked-by link — the one
// structured reason, the same one the band's ⛔ signal names — it waits on
// another item: waiting elsewhere. With none recorded Hive cannot tell a
// person-gate from an outside dependency, so once the issue has gone the stale
// period untouched it asks the person who can lift it; a fresh block keeps
// its earlier decision. Blocked-by links are read for actionable issues only
// (the snapshot's depends_on), so a held or filtered issue with none recorded
// keeps its earlier decision too: "nothing says on what" would be a claim
// about links Hive never read.
func blockedIssueOwnership(in issueOwnershipInput, env ownershipEnv) (Ownership, bool) {
	if !labelSet(in.labels)[blockedWorkflowLabel] {
		return Ownership{}, false
	}
	if len(in.openBlockers) > 0 {
		return waitingElsewhere(), true
	}
	if !in.actionable || !env.stale(in.createdAt, in.updatedAt) {
		return Ownership{}, false
	}
	return yourTurn(OwnershipReasonBlockedUnexplained, "Labelled blocked, nothing says on what — name the blocker or lift it"), true
}

// decidePROwnershipGated is decidePROwnership with the rules for a pull
// request that waits on a person although nothing earlier says so: a gate
// label, a person's review decision, or a merge no Hive lane will make. Held
// PRs keep their hold-source decision.
func decidePROwnershipGated(pr github.PullRequest, verdict *github.MergeVerdict, held bool, cfg config.DashboardIssueBandsConfig, env ownershipEnv) Ownership {
	out := decidePROwnership(pr, held, env)
	if held || !ownershipGateMayDecide(out) {
		return out
	}
	if gated, ok := prGateLabelOwnership(pr, cfg, env); ok {
		return gated
	}
	if reason, blocks := pr.HumanReviewBlocksMerge(); blocks && !pr.Draft {
		return yourTurn(OwnershipReasonReviewDecision, "Waiting on a person's review: "+reason)
	}
	if prNeedsHandMerge(pr, verdict, env) {
		return yourTurn(OwnershipReasonHandMerge, "Hive will not merge this PR; merge it by hand")
	}
	return out
}

// prGateLabelOwnership decides a PR carrying a label that puts it in the
// Gated band (prGateLabels) or in a parking bucket: the bucket's verb as for
// a parked issue, 2-discussing as needs-decision, any other gate label
// REVIEW. needs-human keeps its own rules in decidePROwnership.
func prGateLabelOwnership(pr github.PullRequest, cfg config.DashboardIssueBandsConfig, env ownershipEnv) (Ownership, bool) {
	labels := labelSet(pr.Labels)
	if labels[ownershipLabelNeedsHuman] {
		return Ownership{}, false
	}
	parking := env.parking
	if len(parking) == 0 {
		parking = defaultOwnershipParking()
	}
	for _, p := range parking {
		if p.bucket != config.DefaultNeedsHumanLabel && hasAnyLabel(labels, p.labels) {
			return parkedOwnership(p.bucket), true
		}
	}
	if labels[prDiscussingLabel] {
		return parkedOwnership(config.DefaultNeedsDecisionLabel), true
	}
	for _, label := range prGateLabels(normalizeIssueBandsConfig(cfg)) {
		name := strings.ToLower(strings.TrimSpace(label))
		if name != "" && labels[name] {
			return yourTurn(OwnershipReasonPRGateLabel, "Gated by the "+name+" label — act on it or remove the label"), true
		}
	}
	return Ownership{}, false
}

// prNeedsHandMerge reports a PR the merge classifier calls eligible but whose
// author no Hive merge lane admits (the sweeps' not-app-authored and
// untrusted-author-role skips): only a person merges it. Hive-authored PRs are
// the self-authored lane's own.
func prNeedsHandMerge(pr github.PullRequest, verdict *github.MergeVerdict, env ownershipEnv) bool {
	if verdict == nil || verdict.State != github.MergeVerdictEligible || pr.Draft || env.mergeLaneTakesAuthor == nil {
		return false
	}
	if prAgentRole(pr) != "" || pr.HiveAttributed {
		return false
	}
	return !env.mergeLaneTakesAuthor(pr.Repo, pr.Author)
}
