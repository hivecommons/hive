package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/forgestate"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

const integrationExecutionPromotionGeneration = 1

func executionPredecessor(subject forgestate.IntegrationCandidateSubject) string {
	if len(subject.Prefixes) == 1 {
		return subject.BaseSHA
	}
	return subject.Prefixes[len(subject.Prefixes)-2].Commit
}

func executionTechnicalPassed(proof integration.ExecutionProof) bool {
	return proof.Archive != nil && (proof.State == integration.Passed ||
		proof.State == integration.Superseded && proof.ObservedState == integration.Passed)
}

func (s *Server) promoteExecutionReady(ctx context.Context, store *integration.Store, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, protection string, now time.Time) (bool, error) {
	snapshot := store.ExecutionSnapshot()
	proofs := snapshot.Proofs
	sort.SliceStable(proofs, func(i, j int) bool {
		if len(proofs[i].Subject.Members) == len(proofs[j].Subject.Members) {
			return proofs[i].ObservedAt.Before(proofs[j].ObservedAt)
		}
		return len(proofs[i].Subject.Members) < len(proofs[j].Subject.Members)
	})
	base, err := s.executionDestination(ctx, scope.Repo, scope.Base)
	if err != nil {
		return false, err
	}
	for _, projection := range proofs {
		subject := projection.Subject
		if subject.Repo != scope.Repo || subject.Base != scope.Base ||
			subject.Policy != scope.Policy || subject.WorkflowTree != scope.WorkflowTree {
			continue
		}
		proof, exists := store.ExecutionProof(subject.ID, projection.RunID, projection.Attempt)
		if !exists || !executionTechnicalPassed(proof) {
			continue
		}
		last := subject.Prefixes[len(subject.Prefixes)-1]
		member := subject.Members[len(subject.Members)-1]
		key := "promotion/" + subject.ID + "/" + executionPredecessor(subject) + "/" + scope.AuthorityCommit
		var previous *integration.ExecutionPromotion
		for index := range snapshot.Promotions {
			if snapshot.Promotions[index].MutationKey == key {
				previous = &snapshot.Promotions[index]
				break
			}
		}
		if previous != nil && previous.State == "applied" {
			if scope.Purpose == "qualification" {
				if err := s.recordExecutionQualification(ctx, store, scope, protection, *previous, now); err != nil {
					return false, err
				}
			}
			continue
		}
		// A crash after CAS is reconciled with exactly the same journalled
		// subject, never a new unbound promotion or a different tested tree.
		reconciling := previous != nil && (previous.State == "unknown" || previous.State == "requested") && base == last.Commit
		if base != executionPredecessor(subject) && !reconciling {
			continue
		}
		if previous != nil && previous.State == "rejected" &&
			(previous.Attempt >= cfg.MaxAttempts || now.Sub(previous.ObservedAt) < cfg.Cooldown) {
			continue
		}
		if !reconciling {
			if err := s.verifyExecutionPredecessors(ctx, store, subject); err != nil {
				continue
			}
		}
		parts := strings.SplitN(scope.Repo, "/", 2)
		pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, parts[0], parts[1], member.PR)
		if err != nil {
			return false, err
		}
		if pr.GetHead().GetSHA() != member.Head || pr.GetBase().GetRef() != scope.Base ||
			!strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), scope.Repo) {
			continue
		}
		promotion := integration.ExecutionPromotion{Repo: scope.Repo, Base: scope.Base, SubjectID: subject.ID,
			ExpectedSHA: executionPredecessor(subject), TestedSHA: last.Commit, AuthorityCommit: scope.AuthorityCommit,
			MutationKey: key, State: "requested", PR: member.PR, Head: member.Head, Attempt: 1, ObservedAt: now,
			Reason: "durable exact-subject promotion intent; actual protected CAS not yet acknowledged"}
		if previous != nil {
			promotion.Attempt = previous.Attempt
			if previous.State == "rejected" {
				promotion.Attempt++
			}
		}
		if err := appendExecutionPromotion(store, promotion); err != nil {
			return false, err
		}
		request := ghpkg.IntegrationPromotion{Repo: scope.Repo, Base: scope.Base, ExpectedSHA: promotion.ExpectedSHA,
			TestedSHA: last.Commit, TestedTree: last.Tree, AuthorHead: member.Head,
			HeadRepo: pr.GetHead().GetRepo().GetFullName(), HeadRef: pr.GetHead().GetRef(),
			SubjectID: subject.ID, Policy: scope.Policy, Generation: integrationExecutionPromotionGeneration,
			AuthorityRef: scope.AuthorityRef, AuthorityCommit: scope.AuthorityCommit}
		result, acknowledged, promoteErr := s.deps.GHClient.PromoteIntegrationStepWithAcknowledgement(ctx, request, s.integration.executionBoundary,
			func(ctx context.Context, exact ghpkg.IntegrationPromotion) error {
				return s.executionPromotionGuard(ctx, store, cfg, scope, protection, proof, exact)
			})
		promotion.ObservedAt = time.Now()
		promotion.ProviderAcknowledged = acknowledged
		switch {
		case promoteErr == nil && result.Provenance == last.Commit:
			promotion.State, promotion.Reason = "applied", "actual protected atomic destination+author-head+authority-epoch CAS acknowledged/reconciled"
		case mutation.DefinitelyNotApplied(promoteErr):
			promotion.State, promotion.Reason = "rejected", "live guard definitely refused before effect: "+promoteErr.Error()
		default:
			promotion.State, promotion.Reason = "unknown", "promotion outcome unknown; only exact fenced reconciliation permitted"
			if promoteErr != nil {
				promotion.Reason += ": " + promoteErr.Error()
			}
		}
		if err := appendExecutionPromotion(store, promotion); err != nil {
			return true, err
		}
		if promotion.State == "applied" {
			if scope.Purpose == "qualification" {
				if err := s.recordExecutionQualification(ctx, store, scope, protection, promotion, time.Now()); err != nil {
					return true, err
				}
			}
			return true, nil
		}
		if promotion.State == "unknown" {
			return true, promoteErr
		}
		// An unrelated held member does not freeze independent ready work.
	}
	return false, nil
}

func appendExecutionPromotion(store *integration.Store, promotion integration.ExecutionPromotion) error {
	return store.AppendExecution(fmt.Sprintf("execution/promotion/%s/%s/%d", promotion.MutationKey, promotion.State, promotion.ObservedAt.UnixNano()),
		promotion.ObservedAt, integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Promotion: &promotion})
}

func (s *Server) verifyExecutionPredecessors(ctx context.Context, store *integration.Store, subject forgestate.IntegrationCandidateSubject) error {
	snapshot := store.ExecutionSnapshot()
	parts := strings.SplitN(subject.Repo, "/", 2)
	for index := 0; index+1 < len(subject.Members); index++ {
		member, prefix := subject.Members[index], subject.Prefixes[index]
		known := false
		for _, applied := range snapshot.Promotions {
			if applied.State == "applied" && applied.Repo == subject.Repo && applied.Base == subject.Base &&
				applied.PR == member.PR && applied.Head == member.Head && applied.TestedSHA == prefix.Commit {
				for _, observed := range snapshot.Proofs {
					if observed.Subject.ID == applied.SubjectID && observed.Subject.Policy == subject.Policy &&
						observed.Subject.WorkflowTree == subject.WorkflowTree && len(observed.Subject.Members) == index+1 &&
						observed.Subject.Prefixes[index] == prefix && observed.ObservedState == integration.Passed {
						known = true
						break
					}
				}
			}
		}
		if !known {
			return fmt.Errorf("prefix predecessor %d lacks exact documented applied/provenance lineage", member.PR)
		}
		pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, parts[0], parts[1], member.PR)
		if err != nil {
			return err
		}
		if !pr.GetMerged() || pr.GetMergeCommitSHA() != prefix.Commit {
			return fmt.Errorf("prefix predecessor %d is not an actually merged exact known step", member.PR)
		}
	}
	return nil
}

func (s *Server) executionPromotionGuard(ctx context.Context, store *integration.Store, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, protection string, proof integration.ExecutionProof, request ghpkg.IntegrationPromotion) error {
	if err := s.executionLiveAuthority(ctx, cfg, scope, protection); err != nil {
		return err
	}
	parts := strings.SplitN(scope.Repo, "/", 2)
	repository, _, err := s.deps.GHClient.GoGitHub().Repositories.Get(ctx, parts[0], parts[1])
	if err != nil {
		return err
	}
	if !repository.GetAllowMergeCommit() {
		return fmt.Errorf("repository disallows the exact tested merge-commit protocol; no squash/rebase substitution")
	}
	last := proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1]
	runs, err := s.deps.GHClient.ReadIntegrationExecutionCandidateRuns(ctx, scope.Repo, scope.CandidateWorkflowID, last.Commit)
	if err != nil {
		return err
	}
	if proof.Subject.Claim == nil {
		return fmt.Errorf("candidate has no exact dispatch claim")
	}
	matches := executionRunsNamed(runs, "Integration "+proof.Subject.ID+" "+proof.Subject.Claim.Owner.Nonce)
	if len(matches) != 1 || matches[0].GetID() != proof.RunID || matches[0].GetRunAttempt() != proof.Attempt {
		return fmt.Errorf("exact candidate run/claim is absent, duplicated or superseded; no promotion")
	}
	refreshed, _, err := s.observeExecutionCandidate(ctx, store, scope, proof.Subject, proof.RunID, proof.Attempt, time.Now())
	if err != nil {
		return err
	}
	if !executionTechnicalPassed(refreshed) {
		return fmt.Errorf("exact candidate/attempt no longer has trusted passed evidence")
	}
	if err := s.verifyExecutionPredecessors(ctx, store, proof.Subject); err != nil {
		return err
	}
	base, err := s.executionDestination(ctx, scope.Repo, scope.Base)
	if err != nil {
		return err
	}
	if base != request.ExpectedSHA {
		return fmt.Errorf("exact candidate predecessor moved")
	}
	s.integration.mu.Lock()
	gate := s.integration.executionEvaluator
	s.integration.mu.Unlock()
	if gate == nil {
		return fmt.Errorf("complete normal boot lane promotion evaluator unavailable")
	}
	member := proof.Subject.Members[len(proof.Subject.Members)-1]
	if err := s.executionPromotionWork(store, proof.Subject, time.Now()); err != nil {
		return err
	}
	evidence := automerge.IntegrationBaseEvidence{Repo: scope.Repo, Base: scope.Base, PR: member.PR,
		Head: member.Head, ExpectedSHA: request.ExpectedSHA, TestedSHA: request.TestedSHA, SubjectID: proof.Subject.ID,
		Verify: func(ctx context.Context) error {
			current, err := s.executionDestination(ctx, scope.Repo, scope.Base)
			if err != nil {
				return err
			}
			if current != request.ExpectedSHA {
				return fmt.Errorf("combined tested parent is not the actual current base")
			}
			live, exists := store.ExecutionProof(proof.Subject.ID, proof.RunID, proof.Attempt)
			if !exists || !executionTechnicalPassed(live) || !live.Subject.SameCore(proof.Subject) {
				return fmt.Errorf("combined immutable tested subject changed")
			}
			return nil
		}}
	current, reason, err := gate(automerge.WithIntegrationBaseEvidence(ctx, evidence), scope.Repo, scope.Base, member.PR, member.Head)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("normal live policy refused exact promotion: %s", reason)
	}
	if err := s.executionLiveAuthority(ctx, cfg, scope, protection); err != nil {
		return err
	}
	// Review/hold/head observations invalidate eligibility, not the immutable
	// technical payload. Restore only after this complete live evaluation.
	revision := refreshed.EligibilityRevision
	for _, invalidation := range store.ExecutionSnapshot().Invalidations {
		if invalidation.Repo == scope.Repo && invalidation.Base == scope.Base && invalidation.Revision > revision {
			revision = invalidation.Revision
		}
	}
	refreshed.State, refreshed.EligibilityRevision, refreshed.ObservedAt = integration.Passed, revision, time.Now()
	refreshed.InvalidatedBy, refreshed.Reason = "", "trusted exact candidate plus freshly revalidated complete normal lane/base/authority guards"
	return store.AppendExecution(fmt.Sprintf("execution/revalidated/%s/%d", refreshed.Key(), refreshed.ObservedAt.UnixNano()),
		refreshed.ObservedAt, integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Proof: &refreshed})
}

func (s *Server) recordExecutionQualification(ctx context.Context, store *integration.Store, scope config.IntegrationExecutionScope, protection string, promotion integration.ExecutionPromotion, now time.Time) error {
	if !promotion.ProviderAcknowledged {
		return fmt.Errorf("isolated state was reconciled without a durable successful provider acknowledgement; actor compatibility unproven, new isolated probe required")
	}
	for _, qualified := range store.ExecutionSnapshot().Qualifications {
		if qualified.ID == scope.QualificationID {
			return nil
		}
	}
	parts := strings.SplitN(scope.Repo, "/", 2)
	pr, _, err := s.deps.GHClient.GoGitHub().PullRequests.Get(ctx, parts[0], parts[1], promotion.PR)
	if err != nil {
		return err
	}
	if !pr.GetMerged() || pr.GetMergeCommitSHA() != promotion.TestedSHA {
		return fmt.Errorf("isolated protected CAS applied, but actual exact PR merge acknowledgement still pending; not production qualification")
	}
	current, qualified, err := s.integrationExecutionQualified(ctx, store, scope)
	if err != nil {
		return err
	}
	if !qualified || current != protection {
		return fmt.Errorf("isolated/production protection profile changed after probe")
	}
	receipt := integration.ExecutionQualification{ID: scope.QualificationID, Repo: scope.Repo,
		Base: scope.QualifiedBase, ProbeBase: scope.Base, ExecutorID: scope.ExecutorID,
		AuthorityCommit: scope.AuthorityCommit, RequestActorID: scope.RequestActorID, Policy: scope.Policy,
		WorkflowTree: scope.WorkflowTree, ProtectionDigest: protection, ProfileDigest: scope.ProviderProfileDigest(),
		SubjectID: promotion.SubjectID, TestedSHA: promotion.TestedSHA, MutationKey: promotion.MutationKey, ObservedAt: now}
	return store.AppendExecution("execution/qualification/"+receipt.ID, now,
		integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Qualification: &receipt})
}
