package dashboard

import ghpkg "github.com/kubestellar/hive/v2/pkg/github"

type contributorAdmissionCandidate struct {
	repoFull string
	repoName string
	number   int
}

type contributorAdmissionDecision struct {
	admitted bool
	reason   string
	claim    ghpkg.IssueClaim
}

const contributorAdmissionReasonOpenPRClaim = "open_pr_claim"

// evaluateContributorNeutralAdmission applies repository-global admission
// decisions shared by ReadyQueue and selectTask. Contributor-specific policy,
// ranking, cooldowns, rate limits, role matching, and routing remain in their
// existing downstream paths.
func (h *ContributeWSHub) evaluateContributorNeutralAdmission(candidate contributorAdmissionCandidate) contributorAdmissionDecision {
	claim, claimed := h.issueClaimedByOpenPR(candidate.repoFull, candidate.repoName, candidate.number)
	if claimed {
		return contributorAdmissionDecision{
			reason: contributorAdmissionReasonOpenPRClaim,
			claim:  claim,
		}
	}
	return contributorAdmissionDecision{admitted: true}
}

// issueClaimedByOpenPR reports whether ANY open PR already claims to fix the
// given issue (kubestellar/hive#3768), via the Dependencies.IssueClaimed hook
// into the governor's duplicate-PR claim ledger. Unlike the agent-side
// FilterClaimedIssues — which deliberately honours only hive-authored claims —
// the contribute queue must honour EVERY claim: a human contributor's open PR
// is exactly the "someone is already on it" signal whose absence let the same
// issue be handed to contributor after contributor.
//
// The ledger keys claims by the repo spelling used in the hive config, which
// FrontendRepo.Name preserves; repo.Full is the org-qualified form and is tried
// first since cross-repo `owner/repo#N` closing references key on it. A hub
// with no hook wired (tests, or a hive booted without GitHub credentials)
// reports no claims and selection proceeds exactly as before.
func (h *ContributeWSHub) issueClaimedByOpenPR(repoFull, repoName string, number int) (ghpkg.IssueClaim, bool) {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.IssueClaimed == nil {
		return ghpkg.IssueClaim{}, false
	}
	if claim, ok := h.server.deps.IssueClaimed(repoFull, number); ok {
		return claim, true
	}
	if repoName != "" && repoName != repoFull {
		if claim, ok := h.server.deps.IssueClaimed(repoName, number); ok {
			return claim, true
		}
	}
	return ghpkg.IssueClaim{}, false
}
