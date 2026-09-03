package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── #4247: shared convergence admission for internal agent kicks ──────────────
//
// Contributor offerability and assignment already withhold a dependency-blocked
// or unknown candidate (#3857/#3904), but scheduled and cached manual internal-
// agent kicks render the raw enumerated issue set. This file provides the ONE
// read-only admitted-issue projection the eval-cycle dispatch boundary consumes,
// built from the SAME observer (observeCandidateDependencies) and the SAME pure
// evaluator (convergence.Evaluate) the contributor paths use — one normalized
// observation/evaluator contract, never a reimplementation in scheduler code.
//
// Rollout (maintainer requirement, precedes #4263): callers gate on the
// convergence feature toggle. With mode "off" (default) this projection is
// never invoked — the kick path is entirely inert and unchanged. With mode
// "shadow" the caller computes the projection and LOGS what would have been
// withheld, but always dispatches the raw population — observed, never
// enforced. Enforcement is a later, explicit increment.
//
// Deliberate scope, per the #4247 contract:
//   - CONVERGENCE admission only. The open-PR claim ledger is not consulted
//     here: internal kicks already have their own claim policy
//     (applyDuplicatePRGuard honours hive-authored claims only, #3792), and
//     re-applying the contributor-side any-claim rule would silently change it.
//   - Nothing is cached across passes; every call builds a fresh sweep from
//     current authoritative bead state, so the judgment is level-triggered and
//     reconstructs identically after a restart.
//   - The raw actionable population remains authoritative for governor queue
//     counts/mode/cadence, dashboard status, PR/review dispatch, escalation,
//     and merge eligibility — this projection covers the ISSUE list only.

// ConvergenceKickFinding is one issue the shared convergence admission would
// withhold from internal agent kicks, with the exact Decision that judged it.
type ConvergenceKickFinding struct {
	Issue    ghpkg.Issue
	Decision convergence.Decision
}

// ConvergenceKickProjection evaluates the shared contributor-neutral
// convergence dependency admission for every enumerated issue, on ONE fresh
// ledger sweep, and returns the admitted projection plus the withheld findings.
//
// Read-only and side-effect free: it assigns nothing, mutates nothing, and the
// input slice is never modified (admitted is a new slice). A hub with no
// contribute hub or no bead ledger wired admits everything — identical to the
// contributor path's behaviour in the same state. Non-GitHub-backed items skip
// the GitHub-only bead observer but evaluate source-native dependency edges
// through the same pure convergence evaluator (kubestellar/hive#4730).
func (s *Server) ConvergenceKickProjection(issues []ghpkg.Issue) (admitted []ghpkg.Issue, withheld []ConvergenceKickFinding) {
	admitted, withheld, _ = s.ConvergenceKickProjectionDetailed(issues)
	return admitted, withheld
}

// ConvergenceKickProjectionDetailed is ConvergenceKickProjection plus the
// sweep's ledger-coverage report (#4263 soak telemetry needs the partial-
// coverage fact per pass). Same single sweep, same observer, same evaluator —
// the coverage is projected from the sweep that judged the candidates, so the
// counts and the coverage cannot disagree about the state they saw.
func (s *Server) ConvergenceKickProjectionDetailed(issues []ghpkg.Issue) (admitted []ghpkg.Issue, withheld []ConvergenceKickFinding, coverage AdmissionCoverage) {
	admitted = make([]ghpkg.Issue, 0, len(issues))
	coverage = AdmissionCoverage{Policy: admissionCoveragePolicy}
	if s == nil || s.contributeHub == nil {
		return append(admitted, issues...), nil, coverage
	}
	hub := s.contributeHub
	// One sweep for the whole pass — the same per-pass snapshot discipline the
	// contributor paths use, so every candidate in this projection is judged
	// against one consistent view of current ledger state.
	sweep := hub.newAdmissionSweep()
	coverage = hub.admissionCoverageFromSweep(sweep)
	for _, issue := range issues {
		candidate := kickAdmissionCandidate(issue)
		if hasBlockedWorkflowLabel(candidate.labels) {
			decision := blockedWorkflowAdmissionDecision()
			withheld = append(withheld, ConvergenceKickFinding{
				Issue: issue, Decision: decision.convergence,
			})
			continue
		}
		var observation convergence.Observation
		if candidate.isGitHubBacked() {
			observation = hub.observeCandidateDependencies(sweep, candidate)
		} else {
			observation = observeExternalDependencies(candidate)
		}
		decision := convergence.Evaluate(observation)
		if decision.Admitted {
			admitted = append(admitted, issue)
			continue
		}
		withheld = append(withheld, ConvergenceKickFinding{Issue: issue, Decision: decision})
	}
	return admitted, withheld, coverage
}

// kickAdmissionCandidate normalises one enumerated issue into the shared
// admission candidate shape. Repo carries whatever spelling the enumerator
// recorded; the bare tail is supplied as repoName so bead identity lookup tries
// both spellings, mirroring candidateIdentityKeys' contract for the contributor
// path (the #2648 key-mismatch class of bug, avoided the same way).
func kickAdmissionCandidate(issue ghpkg.Issue) contributorAdmissionCandidate {
	repoName := issue.Repo
	if i := strings.LastIndex(issue.Repo, "/"); i >= 0 {
		repoName = issue.Repo[i+1:]
	}
	return contributorAdmissionCandidate{
		repoFull: issue.Repo,
		repoName: repoName,
		number:   issue.Number,
		ref: worksource.Ref{
			SourceType: issue.SourceType,
			Repo:       issue.Repo,
			ExternalID: issue.ExternalID,
			Number:     issue.Number,
			URL:        issue.URL,
		},
		labels:    issue.Labels,
		dependsOn: issue.DependsOn,
	}
}
