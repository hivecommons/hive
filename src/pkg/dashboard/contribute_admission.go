package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

const blockedWorkflowLabel = "blocked"

// hasBlockedWorkflowLabel reports whether a candidate carries the canonical
// external-dependency overlay. A blocked item is not contributor work until
// its dependency is cleared, so this gate is enforced in both ReadyQueue and
// selectTask rather than relying only on the enumerator's refresh.
func hasBlockedWorkflowLabel(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), blockedWorkflowLabel) {
			return true
		}
	}
	return false
}

func blockedWorkflowAdmissionDecision() contributorAdmissionDecision {
	return contributorAdmissionDecision{
		reason: contributorAdmissionReasonWorkflowBlocked,
		convergence: convergence.Decision{
			Reason: contributorAdmissionReasonWorkflowBlocked,
		},
	}
}

type contributorAdmissionCandidate struct {
	repoFull string
	repoName string
	number   int
	// ref is the candidate's canonical, source-aware identity
	// (kubestellar/hive#4245). repoFull/repoName/number remain for the
	// GitHub-only observers below, which can act on nothing else; ref is what
	// every identity-keyed surface (hold, cooldown, active, order, task key)
	// uses, so a Linear or Jira candidate is no longer indistinguishable from
	// every other zero-numbered item.
	//
	// A zero ref means the caller predates source-aware identity; isGitHubBacked
	// falls back to the number so such a caller behaves exactly as before.
	ref worksource.Ref
	// dependsOn is populated by non-GitHub work-source adapters. It is kept on
	// the candidate rather than inserted into the durable bead ledger: the
	// external source remains authoritative and every sweep re-evaluates its
	// current edge snapshot through the same convergence evaluator.
	dependsOn []ghpkg.IssueDependency
	labels    []string
}

// isGitHubBacked reports whether this candidate may be handed to the two
// deliberately GitHub-only observers: the open-PR claim ledger and the legacy
// bead dependency gate.
//
// Both accept a repository plus a POSITIVE issue number and can infer nothing
// from an external work item. Passing them a Linear or Jira candidate would
// mean synthesising "#0", which either matches an unrelated record or invents
// one — so such a candidate SKIPS both observers instead. That is a deliberate
// scope boundary, not a gap: GitHub is the first operational observer here, not
// the identity ontology.
func (c contributorAdmissionCandidate) isGitHubBacked() bool {
	if c.ref != (worksource.Ref{}) {
		return c.ref.IsGitHubIssue()
	}
	return c.repoFull != "" && c.number > 0
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
	// contributorAdmissionReasonWorkflowBlocked: the canonical workflow state
	// says the work is waiting on an external dependency or human input.
	contributorAdmissionReasonWorkflowBlocked = "workflow_blocked"
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
// Gate order is deliberate for GitHub-backed work: the open-PR claim is checked
// first because it is the cheapest and most specific "someone is already on
// it" signal. External work bypasses that GitHub-only observer and evaluates
// the source-native dependency edges carried by its enumeration snapshot.
func (h *ContributeWSHub) evaluateContributorNeutralAdmission(sweep *contributorAdmissionSweep, candidate contributorAdmissionCandidate) contributorAdmissionDecision {
	if hasBlockedWorkflowLabel(candidate.labels) {
		return blockedWorkflowAdmissionDecision()
	}

	// isGitHubBacked remains the hard boundary around BOTH legacy observers: the
	// PR-claim ledger and bead identity lookup still see only real issue numbers.
	// External work takes a parallel convergence path over the adapter-provided
	// source edges; with no edges it is admitted just as it was before #4730.
	if !candidate.isGitHubBacked() {
		return admissionDecisionFromConvergence(convergence.Evaluate(observeExternalDependencies(candidate)))
	}

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
	return admissionDecisionFromConvergence(convergence.Evaluate(h.observeCandidateDependencies(sweep, candidate)))
}

func admissionDecisionFromConvergence(decision convergence.Decision) contributorAdmissionDecision {
	if !decision.Admitted {
		reason := contributorAdmissionReasonDependencyBlocked
		if decision.Reason != convergence.ReasonWaitingForDependency {
			reason = contributorAdmissionReasonDependencyUnknown
		}
		return contributorAdmissionDecision{reason: reason, convergence: decision}
	}
	return contributorAdmissionDecision{admitted: true, convergence: decision}
}

func observeExternalDependencies(candidate contributorAdmissionCandidate) convergence.Observation {
	obs := convergence.Observation{
		Subject:  convergence.Subject{WorkKey: candidate.ref.Key()},
		Found:    true,
		RecordID: candidate.ref.Key(),
	}
	for _, edge := range candidate.dependsOn {
		status := convergence.ConditionFalse
		detail := "external dependency is still open"
		if edge.Resolved {
			status = convergence.ConditionTrue
			detail = "external dependency is resolved"
		}
		if _, ok := worksource.ParseKey(edge.Key); !ok {
			status = convergence.ConditionUnknown
			detail = "external dependency has no canonical work identity"
		}
		obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
			ID: edge.Key, Status: status, Detail: detail,
		})
	}
	return obs
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
