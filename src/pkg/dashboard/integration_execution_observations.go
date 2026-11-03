package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/forgestate"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	integrationExecutionMaterializerPath      = ".github/workflows/integration-candidate-prepare.yml"
	integrationExecutionCandidatePath         = ".github/workflows/integration-candidate.yml"
	integrationExecutionMaterializerFinalizer = "integration materializer receipt"
	integrationExecutionCandidateFinalizer    = "integration producer receipt"
)

func executionRunsNamed(runs []*gh.WorkflowRun, name string) []*gh.WorkflowRun {
	out := []*gh.WorkflowRun{}
	for _, run := range runs {
		if run != nil && run.GetDisplayTitle() == name {
			out = append(out, run)
		}
	}
	return out
}

func (s *Server) observeExecutionPlans(ctx context.Context, store *integration.Store, cfg config.IntegrationExecutionConfig, scope config.IntegrationExecutionScope, now time.Time, budget *int) error {
	from := now.Add(-cfg.Window - cfg.ObservationAge)
	plans := []integration.ExecutionPlan{}
	knownRequests, knownCandidates := map[string]bool{}, map[string]bool{}
	for _, projected := range store.ExecutionSnapshot().Plans {
		plan, exists := store.ExecutionPlan(projected.Recipe.ID())
		if !exists {
			continue
		}
		if plan.RequestDigest != "" {
			knownRequests["Prepare integration "+plan.RequestDigest] = true
		}
		for _, subject := range plan.Prepared {
			if subject.Claim != nil {
				knownCandidates["Integration "+subject.ID+" "+subject.Claim.Owner.Nonce] = true
			}
		}
		if plan.Recipe.Repo != scope.Repo || plan.Recipe.Base != scope.Base {
			continue
		}
		active := plan.State == integration.Unknown || plan.State == integration.Waiting || plan.State == integration.Running
		if !active && plan.CreatedAt.Before(from) {
			continue
		}
		if active && plan.CreatedAt.Before(from) {
			from = plan.CreatedAt.Add(-time.Minute)
		}
		plans = append(plans, plan)
	}
	complete, reason := true, "all durable owned producer/bootstrap episodes observed"
	var materializers, candidates []*gh.WorkflowRun
	var discoveryErr error
	materializers, discoveryErr = s.deps.GHClient.ReadIntegrationExecutionRuns(ctx, scope.Repo, scope.MaterializerID, from)
	if discoveryErr == nil {
		candidates, discoveryErr = s.deps.GHClient.ReadIntegrationExecutionRuns(ctx, scope.Repo, scope.CandidateWorkflowID, from)
	}
	if discoveryErr != nil {
		complete, reason = false, discoveryErr.Error()
	}
	// Unknown producer deliveries are never assumed free. This also exposes
	// bootstrap/dispatch duplicates instead of claiming exactly-once execution.
	for _, cohort := range []struct {
		runs  []*gh.WorkflowRun
		known map[string]bool
	}{
		{materializers, knownRequests}, {candidates, knownCandidates},
	} {
		counts := map[string]int{}
		for _, run := range cohort.runs {
			counts[run.GetDisplayTitle()]++
		}
		for _, run := range cohort.runs {
			if !cohort.known[run.GetDisplayTitle()] || counts[run.GetDisplayTitle()] != 1 {
				complete, reason = false, "unowned/ambiguous installed producer run requires owner reconciliation"
			}
		}
	}
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].ObservedAt.Before(plans[j].ObservedAt) })
	for _, plan := range plans {
		if plan.RequestDigest == "" {
			continue
		} // durable plan, no pre-effect intent yet
		matches := executionRunsNamed(materializers, "Prepare integration "+plan.RequestDigest)
		if plan.DispatchOutcome == "not-applied" && len(matches) == 0 && discoveryErr == nil {
			continue
		}
		if discoveryErr != nil || len(matches) != 1 {
			complete, reason = false, "preparation has no unique actual run; no absence-based redispatch"
			continue
		}
		run := matches[0]
		if !executionTerminalMaterializer(plan, run) {
			if *budget <= 0 {
				complete, reason = false, "bounded run observation budget exhausted"
				break
			}
			*budget--
			observed, err := s.deps.GHClient.ReadIntegrationMaterializerObservation(ctx, ghpkg.IntegrationMaterializerExpectation{
				Request: executionPreparationRequest(plan), RequestDigest: plan.RequestDigest,
				DefaultBranch: scope.DefaultBranch, EventName: "repository_dispatch", WorkflowTree: scope.WorkflowTree,
				RepositoryID: scope.RepositoryID, ActorID: scope.RequestActorID, WorkflowID: scope.MaterializerID,
				CheckAppID: scope.ProducerAppID, RunID: run.GetID(), Attempt: run.GetRunAttempt(),
			}, s.integration.executionVerifier, now)
			if jobErr := appendExecutionJobs(store, scope, run.GetHeadSHA(), "materializer", []string{plan.Recipe.ID()}, observed.Evidence, now); jobErr != nil {
				return jobErr
			}
			if observed.Evidence.JobCompleteness != "complete" {
				complete, reason = false, "actual bootstrap job cohort incomplete"
			}
			s.rememberExecutionCost(scope.Repo, observed.Evidence, now)
			plan.Generation++
			plan.JobCompleteness = observed.Evidence.JobCompleteness
			plan.ObservedAt, plan.MaterializerRunID, plan.MaterializerAttempt = now, run.GetID(), run.GetRunAttempt()
			if err != nil {
				plan.State, plan.Reason = integration.Unknown, "materializer provenance unresolved: "+err.Error()
				complete, reason = false, plan.Reason
			} else {
				plan.State, plan.Reason = integration.State(observed.Evidence.State), observed.Evidence.Detail
				if observed.Evidence.State == "passed" {
					archive, archiveErr := executionArchive(scope, observed.Evidence, "materializer",
						observed.Receipt.PreparedBy.SourceSHA, "refs/heads/"+scope.DefaultBranch, "repository_dispatch")
					if archiveErr != nil {
						return archiveErr
					}
					plan.Materialization, plan.Prepared = archive, []forgestate.IntegrationCandidateSubject{}
					for _, prepared := range observed.Receipt.Prepared {
						if prepared.Publication == "accepted" {
							plan.Prepared = append(plan.Prepared, prepared.Subject)
						}
					}
					plan.State, plan.Reason = integration.Waiting, "materialization verified; exact candidate workflows pending"
					if len(plan.Prepared) == 0 {
						plan.State, plan.Reason = integration.Rejected, "trusted materializer produced no accepted prefix"
					}
				}
			}
			if err := appendExecutionPlan(store, plan); err != nil {
				return err
			}
		}
		allTerminal, allPassed := len(plan.Prepared) > 0, len(plan.Prepared) > 0
		subjects := append([]forgestate.IntegrationCandidateSubject(nil), plan.Prepared...)
		lastObserved := map[string]time.Time{}
		for _, proof := range store.ExecutionSnapshot().Proofs {
			if proof.ObservedAt.After(lastObserved[proof.Subject.ID]) {
				lastObserved[proof.Subject.ID] = proof.ObservedAt
			}
		}
		sort.SliceStable(subjects, func(i, j int) bool {
			return lastObserved[subjects[i].ID].Before(lastObserved[subjects[j].ID])
		})
		for _, subject := range subjects {
			if subject.Claim == nil {
				complete, reason, allTerminal = false, "candidate has no verified initial claim", false
				continue
			}
			matches := executionRunsNamed(candidates, "Integration "+subject.ID+" "+subject.Claim.Owner.Nonce)
			if len(matches) != 1 {
				complete, reason, allTerminal = false, "candidate run absent/duplicated; owner action required, no rerun", false
				continue
			}
			run := matches[0]
			proof, cached := store.ExecutionProof(subject.ID, run.GetID(), run.GetRunAttempt())
			if cached && executionTerminalCandidate(proof, run) {
				allTerminal = allTerminal && (proof.ObservedState == integration.Passed || proof.ObservedState == integration.Rejected)
				allPassed = allPassed && proof.ObservedState == integration.Passed
				continue
			}
			if *budget <= 0 {
				complete, reason, allTerminal = false, "bounded candidate observation budget exhausted", false
				break
			}
			*budget--
			proof, cohortComplete, err := s.observeExecutionCandidate(ctx, store, scope, subject, run.GetID(), run.GetRunAttempt(), now)
			if err != nil {
				complete, reason = false, err.Error()
			}
			complete = complete && cohortComplete
			allTerminal = allTerminal && (proof.State == integration.Passed || proof.State == integration.Rejected)
			allPassed = allPassed && proof.State == integration.Passed
		}
		if len(plan.Prepared) > 0 {
			plan.Generation++
			plan.State = integration.Running
			if allTerminal {
				plan.State = integration.Rejected
				if allPassed && len(plan.Prepared) == len(plan.Recipe.Members) {
					plan.State = integration.Passed
				}
			}
			plan.Reason = "actual exact-prefix technical workflow observations; author eligibility remains separate"
			if allTerminal && len(plan.Prepared) != len(plan.Recipe.Members) {
				plan.Reason = "only a strict prefix was published; recipe tail rejected, valid prefix receipts retained independently"
			}
			if err := appendExecutionPlan(store, plan); err != nil {
				return err
			}
		}
	}
	allRuns := append(append([]*gh.WorkflowRun(nil), materializers...), candidates...)
	if err := s.reconcileExecutionCostHistory(ctx, store, scope, allRuns, now, budget); err != nil {
		complete, reason = false, err.Error()
	}
	coverage := integration.Coverage{Repo: scope.Repo, Base: scope.Base, From: from, Through: now,
		Complete: complete, Source: "execution-owned-actions-cohort-v2", Reason: reason}
	if err := store.Append(integration.Event{Version: integration.SchemaVersion,
		ID: fmt.Sprintf("execution/coverage/%s/%s/%d", scope.Repo, scope.Base, now.UnixNano()), At: now, Coverage: &coverage}); err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("%s", reason)
	}
	return nil
}

func executionArchive(scope config.IntegrationExecutionScope, observed ghpkg.IntegrationCandidateObservation, role, workflowSHA, ref, event string) (*integration.ExecutionArchive, error) {
	path, finalizer, actor := integrationExecutionCandidatePath, integrationExecutionCandidateFinalizer, scope.CandidateActorID
	if role == "materializer" {
		path, finalizer, actor = integrationExecutionMaterializerPath, integrationExecutionMaterializerFinalizer, scope.RequestActorID
	}
	archive := &integration.ExecutionArchive{Role: role, Issuer: observed.Identity.Issuer, KeyID: observed.Identity.KeyID,
		SigningKeySPKI: observed.Identity.SigningKeySPKI, SigningKeySHA256: observed.Identity.SigningKeySHA256,
		VerifiedAt: observed.Identity.VerifiedAt, Payload: observed.Payload, Token: observed.Token,
		ArtifactID: observed.ArtifactID, ArtifactDigest: observed.ArtifactDigest, WorkflowID: observed.WorkflowID,
		SuiteID: observed.SuiteID, CheckIDs: observed.CheckIDs, FinalizerCheckID: observed.Identity.FinalizerCheckRunID,
		RepositoryID: scope.RepositoryID, ActorID: actor, WorkflowSHA: workflowSHA, WorkflowPath: path, Ref: ref, Event: event}
	found := false
	for _, job := range observed.Jobs {
		if job.Name == finalizer {
			if found {
				return nil, fmt.Errorf("actual isolated finalizer job ambiguous")
			}
			found, archive.JobStartedAt, archive.JobCompletedAt = true, job.StartedAt, job.CompletedAt
		}
	}
	if !found {
		return nil, fmt.Errorf("actual isolated finalizer interval unavailable")
	}
	return archive, nil
}

func appendExecutionJobs(store *integration.Store, scope config.IntegrationExecutionScope, head, role string, subjects []string, observed ghpkg.IntegrationCandidateObservation, now time.Time) error {
	for _, actual := range observed.Jobs {
		job := integration.Job{Repo: scope.Repo, Base: scope.Base, ID: actual.JobID, RunID: actual.RunID,
			Attempt: actual.Attempt, Head: head, Workflow: role, RunnerID: actual.RunnerID, Status: actual.Status,
			Conclusion: actual.Conclusion, StartedAt: actual.StartedAt, CompletedAt: actual.CompletedAt,
			ObservedAt: actual.ObservedAt, Candidates: subjects}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion,
			ID: fmt.Sprintf("execution/job/%s/%d", job.Key(), now.UnixNano()), At: now, Job: &job}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) observeExecutionCandidate(ctx context.Context, store *integration.Store, scope config.IntegrationExecutionScope, subject forgestate.IntegrationCandidateSubject, runID int64, attempt int, now time.Time) (integration.ExecutionProof, bool, error) {
	observed, err := s.deps.GHClient.ReadIntegrationCandidateObservation(ctx, ghpkg.IntegrationCandidateExpectation{
		Subject: subject, RepositoryID: scope.RepositoryID, ActorID: scope.CandidateActorID,
		WorkflowID: scope.CandidateWorkflowID, CheckAppID: scope.ProducerAppID,
		RunID: runID, Attempt: attempt, RequiredJobs: scope.RequiredJobs,
	}, s.integration.executionVerifier, now)
	last := subject.Prefixes[len(subject.Prefixes)-1]
	if jobErr := appendExecutionJobs(store, scope, last.Commit, "candidate", []string{subject.ID}, observed, now); jobErr != nil {
		return integration.ExecutionProof{}, false, jobErr
	}
	proof := integration.ExecutionProof{Subject: subject, State: integration.State(observed.State), ObservedAt: now,
		Source: "github-actions-oidc-v1", RunID: runID, Attempt: attempt, Reason: observed.Detail, JobCompleteness: observed.JobCompleteness}
	s.rememberExecutionCost(scope.Repo, observed, now)
	if err != nil {
		proof.State, proof.Reason = integration.Unknown, err.Error()
	}
	if observed.Identity.VerifiedAt.IsZero() == false && len(observed.Payload) > 0 {
		archive, archiveErr := executionArchive(scope, observed, "candidate", last.Commit, "refs/tags/hive-integration/"+subject.ID, "workflow_dispatch")
		if archiveErr != nil {
			return proof, false, archiveErr
		}
		proof.Archive = archive
	}
	// Preserve invalidation until a complete live promotion evaluation restores
	// eligibility; a repeated artifact observation is not a review receipt.
	if old, exists := store.ExecutionProof(subject.ID, runID, attempt); exists {
		proof.EligibilityRevision = old.EligibilityRevision
	}
	if appendErr := store.AppendExecution(fmt.Sprintf("execution/proof/%s/%d", proof.Key(), now.UnixNano()), now,
		integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Proof: &proof}); appendErr != nil {
		return proof, false, appendErr
	}
	persisted, _ := store.ExecutionProof(subject.ID, runID, attempt)
	return persisted, observed.JobCompleteness == "complete", err
}

func (s *Server) observeIntegrationExecutionWebhook(event, delivery string, body []byte, now time.Time) error {
	cfg := s.integrationConfig().Execution.Effective()
	if cfg.Mode == config.IntegrationOff {
		return nil
	}
	var payload integrationWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	enrolled := false
	for _, scope := range cfg.Scopes {
		if strings.EqualFold(scope.Repo, payload.Repository.FullName) {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return nil
	}
	store, err := s.integrationExecutionStore()
	if err != nil {
		return err
	}
	if store.HasEvent("execution/github/" + delivery) {
		return nil
	}
	if (event == "pull_request" || event == "pull_request_review") && payload.PullRequest != nil && !payload.PullRequest.GetMerged() {
		pr := payload.PullRequest
		invalidation := integration.ExecutionInvalidation{Repo: payload.Repository.FullName,
			Base: pr.GetBase().GetRef(), PR: pr.GetNumber(), Revision: uint64(now.UnixNano()), Reason: "live head/member/hold/review changed"}
		if payload.Action == "synchronize" {
			invalidation.Head = pr.GetHead().GetSHA()
		}
		if err := store.AppendExecution("execution/github/"+delivery, now,
			integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Invalidate: &invalidation}); err != nil {
			return err
		}
	}
	policyChanged := event == "branch_protection_rule" || event == "repository_ruleset"
	if policyChanged {
		for _, scope := range cfg.Scopes {
			if !strings.EqualFold(scope.Repo, payload.Repository.FullName) {
				continue
			}
			id := "execution/github/" + delivery + "/policy/" + scope.Base
			if store.HasEvent(id) {
				continue
			}
			invalidation := integration.ExecutionInvalidation{Repo: scope.Repo, Base: scope.Base, Revision: uint64(now.UnixNano()),
				Reason: "actual protection/ruleset event changed eligibility; exact live policy revalidation required"}
			if err := store.AppendExecution(id, now, integration.ExecutionEvent{Version: integration.ExecutionSchemaVersion, Invalidate: &invalidation}); err != nil {
				return err
			}
		}
	}
	if policyChanged || event == "workflow_run" || event == "workflow_job" || event == "pull_request" || event == "pull_request_review" || event == "push" {
		s.integration.mu.Lock()
		if now.Sub(s.integration.executionLastNudge) >= config.MinIntegrationExecutionInterval {
			s.integration.executionLastReconcile = time.Time{}
			s.integration.executionLastNudge = now
		}
		s.integration.mu.Unlock()
	}
	return nil
}
