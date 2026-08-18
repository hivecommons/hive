package dashboard

import (
	"github.com/kubestellar/hive/pkg/convergence"
	ghpkg "github.com/kubestellar/hive/pkg/github"
)

type contributorAdmissionCandidate struct {
	repoFull string
	repoName string
	number   int
}

type contributorAdmissionDecision struct {
	admitted bool
	reason   string
	claim    ghpkg.IssueClaim
	// convergence carries the dependency judgment behind a dependency-based
	// refusal (#3845): which record was observed, at which generation, and
	// which dependency IDs blocked. Zero-valued when admission never reached
	// the dependency gate (an open-PR claim short-circuits first).
	convergence convergence.Decision
}

const (
	contributorAdmissionReasonOpenPRClaim = "open_pr_claim"
	// contributorAdmissionReasonDependencyBlocked: a declared dependency is
	// established as NOT satisfied.
	contributorAdmissionReasonDependencyBlocked = "dependency_blocked"
	// contributorAdmissionReasonDependencyUnknown: a declared dependency could
	// not be resolved, so satisfaction cannot be asserted and the candidate is
	// not mutably dispatched.
	contributorAdmissionReasonDependencyUnknown = "dependency_unknown"
)

// evaluateContributorNeutralAdmission applies repository-global admission
// decisions shared by ReadyQueue and selectTask. Contributor-specific policy,
// ranking, cooldowns, rate limits, role matching, and routing remain in their
// existing downstream paths.
//
// sweep is the per-pass ledger snapshot the caller built once before iterating
// candidates (see newAdmissionSweep). A nil sweep is legal and builds a
// single-use snapshot inline — correct, just wasteful — so a future call site
// that forgets to thread one through gets the right answer rather than
// accidentally skipping the dependency gate.
//
// Gate order is deliberate: the open-PR claim is checked first because it is the
// cheapest and most specific "someone is already on it" signal, and short-
// circuiting it keeps its existing log line and reason unchanged.
func (h *ContributeWSHub) evaluateContributorNeutralAdmission(sweep *contributorAdmissionSweep, candidate contributorAdmissionCandidate) contributorAdmissionDecision {
	claim, claimed := h.issueClaimedByOpenPR(candidate.repoFull, candidate.repoName, candidate.number)
	if claimed {
		return contributorAdmissionDecision{
			reason: contributorAdmissionReasonOpenPRClaim,
			claim:  claim,
		}
	}

	// Dependency-aware convergence admission (#3845). The judgment is recomputed
	// from current ledger state on every sweep, so satisfaction changes take
	// effect without a restart or manual queue surgery, and a dependency that
	// becomes unsatisfied again removes the dependent from the ready set.
	if sweep == nil {
		sweep = h.newAdmissionSweep()
	}
	decision := convergence.Evaluate(h.observeCandidateDependencies(sweep, candidate))
	if !decision.Admitted {
		reason := contributorAdmissionReasonDependencyBlocked
		if decision.Reason != convergence.ReasonWaitingForDependency {
			reason = contributorAdmissionReasonDependencyUnknown
		}
		return contributorAdmissionDecision{reason: reason, convergence: decision}
	}
	return contributorAdmissionDecision{admitted: true, convergence: decision}
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
