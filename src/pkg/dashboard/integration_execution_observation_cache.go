package dashboard

import (
	"context"
	"fmt"
	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"sort"
	"time"
)

const (
	integrationExecutionMaxRunAttempts = 100
	integrationExecutionMaxCostCache   = 512
)

type executionCostObservation struct {
	ObservedAt time.Time
	Terminal   bool
}

func executionTerminalMaterializer(plan integration.ExecutionPlan, run *gh.WorkflowRun) bool {
	return plan.Materialization != nil && plan.JobCompleteness == "complete" &&
		plan.MaterializerRunID == run.GetID() && plan.MaterializerAttempt == run.GetRunAttempt() &&
		run.GetStatus() == "completed" && run.GetHeadSHA() == plan.Materialization.WorkflowSHA &&
		!run.GetUpdatedAt().Time.After(plan.Materialization.VerifiedAt)
}

func executionTerminalCandidate(proof integration.ExecutionProof, run *gh.WorkflowRun) bool {
	return proof.Archive != nil && proof.JobCompleteness == "complete" &&
		proof.RunID == run.GetID() && proof.Attempt == run.GetRunAttempt() &&
		run.GetStatus() == "completed" &&
		!run.GetUpdatedAt().Time.After(proof.Archive.VerifiedAt) &&
		(proof.ObservedState == integration.Passed && run.GetConclusion() == "success" ||
			proof.ObservedState == integration.Rejected && run.GetConclusion() != "success")
}

func executionCostObservationKey(repo string, runID int64, attempt int) string {
	return fmt.Sprintf("%s/%d/%d", repo, runID, attempt)
}

func (s *Server) rememberExecutionCost(repo string, observed ghpkg.IntegrationCandidateObservation, now time.Time) {
	if observed.JobCompleteness != "complete" {
		return
	}
	terminal := len(observed.Jobs) > 0
	for _, job := range observed.Jobs {
		terminal = terminal && job.Status == "completed"
	}
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	if s.integration.executionCostObservations == nil {
		s.integration.executionCostObservations = map[string]executionCostObservation{}
	}
	if len(s.integration.executionCostObservations) >= integrationExecutionMaxCostCache {
		oldestKey, oldest := "", now
		for key, current := range s.integration.executionCostObservations {
			if oldestKey == "" || current.ObservedAt.Before(oldest) {
				oldestKey, oldest = key, current.ObservedAt
			}
		}
		delete(s.integration.executionCostObservations, oldestKey)
	}
	s.integration.executionCostObservations[executionCostObservationKey(repo, observed.RunID, observed.Attempt)] =
		executionCostObservation{ObservedAt: now, Terminal: terminal}
}

// Completeness is deliberately rebuilt after restart. Persisted physical job
// facts remain; an empty in-memory cache is not evidence that a missed attempt
// or duplicate delivery consumed zero runner time.
func (s *Server) reconcileExecutionCostHistory(ctx context.Context, store *integration.Store, scope config.IntegrationExecutionScope, runs []*gh.WorkflowRun, now time.Time, budget *int) error {
	type pending struct {
		run        *gh.WorkflowRun
		attempt    int
		observedAt time.Time
	}
	missing := []pending{}
	s.integration.mu.Lock()
	for _, run := range runs {
		if run.GetRunAttempt() < 1 || run.GetRunAttempt() > integrationExecutionMaxRunAttempts {
			s.integration.mu.Unlock()
			return fmt.Errorf("actual run attempt history outside bounded accounting; owner reconciliation required")
		}
		for attempt := 1; attempt <= run.GetRunAttempt(); attempt++ {
			last, known := s.integration.executionCostObservations[executionCostObservationKey(scope.Repo, run.GetID(), attempt)]
			if !known || !last.Terminal && last.ObservedAt.Before(now) {
				missing = append(missing, pending{run, attempt, last.ObservedAt})
			}
		}
	}
	s.integration.mu.Unlock()
	sort.SliceStable(missing, func(i, j int) bool { return missing[i].observedAt.Before(missing[j].observedAt) })
	for _, current := range missing {
		if *budget <= 0 {
			return fmt.Errorf("bounded actual duplicate/attempt job accounting incomplete; unknown is not zero")
		}
		*budget--
		observed, err := s.deps.GHClient.ReadIntegrationExecutionJobs(ctx, scope.Repo, current.run.GetWorkflowID(), current.run.GetID(), current.attempt, now)
		if appendErr := appendExecutionJobs(store, scope, current.run.GetHeadSHA(), "producer-attempt-accounting", nil, observed, now); appendErr != nil {
			return appendErr
		}
		s.rememberExecutionCost(scope.Repo, observed, now)
		if err != nil {
			return err
		}
	}
	return nil
}
