package integration

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ResumeExecution permits only an unissued plan or a definitely-not-applied
// request. Pending/unknown effects never become stealable after TTL/cooldown.
func (s *Store) ResumeExecution(cfg config.IntegrationExecutionConfig, id string, observed Cost, now time.Time) (ExecutionPlan, error) {
	cfg = cfg.Effective()
	if cfg.Mode != config.IntegrationEnforce || cfg.Validate() != nil || !executionCostKnown(observed) {
		return ExecutionPlan{}, fmt.Errorf("safe resume requires current enforce profile and complete actual accounting")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return ExecutionPlan{}, err
	}
	defer unlock()
	state := s.executionData()
	plan, exists := state.plans[id]
	if !exists {
		return ExecutionPlan{}, fmt.Errorf("safe resume plan missing")
	}
	scope, enrolled := cfg.Scope(plan.Recipe.Repo, plan.Recipe.Base)
	if !enrolled || scope.ExecutorID != plan.ExecutorID || scope.AuthorityCommit != plan.AuthorityCommit {
		return ExecutionPlan{}, fmt.Errorf("safe resume authority changed")
	}
	unissued := plan.RequestDigest == "" && plan.State == Waiting
	retry := plan.State == Rejected && plan.DispatchOutcome == "not-applied" &&
		plan.Attempt < cfg.MaxAttempts && now.Sub(plan.ObservedAt) >= cfg.Cooldown
	if !unissued && !retry {
		return ExecutionPlan{}, fmt.Errorf("request is not definitely unissued/not-applied; no blind retry")
	}
	active, jobs, reserved := len(plan.Recipe.Members), len(plan.Recipe.Members)*scope.ExpectedJobs,
		float64(len(plan.Recipe.Members))*scope.ExpectedMinutes
	for otherID, other := range state.plans {
		if otherID == id || other.ReservationsReleased || other.State != Unknown && other.State != Waiting && other.State != Running {
			continue
		}
		profile, enrolled := cfg.Scope(other.Recipe.Repo, other.Recipe.Base)
		if !enrolled {
			continue
		}
		active += len(other.Recipe.Members)
		jobs += len(other.Recipe.Members) * profile.ExpectedJobs
		reserved += float64(len(other.Recipe.Members)) * profile.ExpectedMinutes
	}
	if active > cfg.MaxActive || observed.Running+jobs > cfg.MaxJobs ||
		*observed.RunnerMinutes+reserved > cfg.MaxRunnerMinutes {
		return ExecutionPlan{}, fmt.Errorf("safe resume global static budget exhausted")
	}
	if retry {
		plan.Attempt++
	}
	plan.Generation++
	plan.AttemptAt, plan.ObservedAt = now, now
	plan.State, plan.Reason, plan.DispatchOutcome = Waiting, "bounded definitely-not-applied/unissued resume reserved", ""
	plan.ReservationsReleased = false
	plan.Materialization, plan.Prepared = nil, nil
	plan.MaterializerRunID, plan.MaterializerAttempt = 0, 0
	plan.RequestDigest, plan.MutationKey = "", ""
	err = s.appendLocked(Event{Version: SchemaVersion, ID: fmt.Sprintf("execution/resume/%s/%d", id, plan.Generation), At: now,
		Execution: &ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}})
	return cloneExecution(plan), err
}
