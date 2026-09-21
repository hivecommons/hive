package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

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
		if label, ok := s.contributeSkipLabel(candidate.labels); ok {
			decision := labelSkippedAdmissionDecision(label)
			if strings.EqualFold(strings.TrimSpace(label), blockedWorkflowLabel) {
				decision = blockedWorkflowAdmissionDecision(label)
			}
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
