package dashboard

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	"github.com/hivecommons/hive/pkg/forgestate"
	"sort"
	"time"
)

type IntegrationExecutionTracker struct {
	Version               int                                    `json:"version"`
	Mode                  string                                 `json:"mode"`
	RuntimeVersion        string                                 `json:"runtime_version"`
	SourceVersion         string                                 `json:"source_version"`
	PublishedImage        string                                 `json:"published_image"`
	ObservedAt            time.Time                              `json:"observed_at"`
	ObservationAgeSeconds float64                                `json:"observation_age_seconds"`
	Telemetry             integration.Cost                       `json:"telemetry"`
	Statuses              []integration.ExecutionStatus          `json:"statuses"`
	Candidates            []IntegrationExecutionTrackerCandidate `json:"candidates"`
}

type IntegrationExecutionTrackerCandidate struct {
	Kind         string                                  `json:"kind"`
	SubjectID    string                                  `json:"subject_id"`
	Repo         string                                  `json:"repo"`
	Base         string                                  `json:"base"`
	BaseSHA      string                                  `json:"base_sha"`
	Members      []forgestate.IntegrationCandidateMember `json:"members"`
	TestedCommit string                                  `json:"tested_commit,omitempty"`
	TestedTree   string                                  `json:"tested_tree,omitempty"`
	State        integration.State                       `json:"state"`
	RunID        int64                                   `json:"run_id,omitempty"`
	Attempt      int                                     `json:"attempt"`
	Reason       string                                  `json:"reason"`
	ObservedAt   time.Time                               `json:"observed_at"`
}

func (s *Server) refreshExecutionTracker(store *integration.Store, cfg config.IntegrationExecutionConfig, now time.Time) {
	snapshot := store.ExecutionSnapshot()
	view := &IntegrationExecutionTracker{Version: integration.ExecutionSchemaVersion, Mode: cfg.Effective().Mode,
		RuntimeVersion: versionHash, SourceVersion: "unknown", PublishedImage: "unknown", ObservedAt: now,
		ObservationAgeSeconds: cfg.ObservationAge.Seconds(),
		Telemetry:             s.integrationExecutionCost(cfg, store, now),
		Candidates:            []IntegrationExecutionTrackerCandidate{}}
	for _, status := range snapshot.Statuses {
		if _, enrolled := cfg.Scope(status.Repo, status.Base); enrolled {
			view.Statuses = append(view.Statuses, status)
		}
	}
	latest := map[string]integration.ExecutionProof{}
	for _, proof := range snapshot.Proofs {
		scope, enrolled := cfg.Scope(proof.Subject.Repo, proof.Subject.Base)
		if !enrolled || scope.Authority != "hive" || scope.Policy != proof.Subject.Policy || scope.WorkflowTree != proof.Subject.WorkflowTree {
			continue
		}
		old, exists := latest[proof.Subject.ID]
		if !exists || proof.Attempt > old.Attempt || proof.Attempt == old.Attempt && proof.ObservedAt.After(old.ObservedAt) {
			latest[proof.Subject.ID] = proof
		}
	}
	for _, proof := range latest {
		subject := proof.Subject
		last := subject.Prefixes[len(subject.Prefixes)-1]
		view.Candidates = append(view.Candidates, IntegrationExecutionTrackerCandidate{
			Kind: "candidate", SubjectID: subject.ID, Repo: subject.Repo, Base: subject.Base,
			BaseSHA: subject.BaseSHA, Members: subject.Members, TestedCommit: last.Commit, TestedTree: last.Tree,
			State: proof.State, RunID: proof.RunID, Attempt: proof.Attempt, Reason: proof.Reason, ObservedAt: proof.ObservedAt})
	}
	for _, plan := range snapshot.Plans {
		scope, enrolled := cfg.Scope(plan.Recipe.Repo, plan.Recipe.Base)
		if !enrolled || scope.Authority != "hive" || scope.Policy != plan.Recipe.Policy || scope.WorkflowTree != plan.Recipe.WorkflowTree || len(plan.Prepared) > 0 {
			continue
		}
		view.Candidates = append(view.Candidates, IntegrationExecutionTrackerCandidate{
			Kind: "preparation", SubjectID: plan.Recipe.ID(), Repo: plan.Recipe.Repo, Base: plan.Recipe.Base,
			BaseSHA: plan.Recipe.BaseSHA, Members: plan.Recipe.Members, State: plan.State,
			RunID: plan.MaterializerRunID, Attempt: plan.Attempt, Reason: plan.Reason, ObservedAt: plan.ObservedAt})
	}
	rank := func(state integration.State) int {
		if state == integration.Unknown || state == integration.Waiting || state == integration.Running {
			return 0
		}
		if state == integration.Passed {
			return 1
		}
		return 2
	}
	sort.SliceStable(view.Candidates, func(i, j int) bool {
		a, b := view.Candidates[i], view.Candidates[j]
		if rank(a.State) != rank(b.State) {
			return rank(a.State) < rank(b.State)
		}
		if !a.ObservedAt.Equal(b.ObservedAt) {
			return a.ObservedAt.After(b.ObservedAt)
		}
		return a.SubjectID < b.SubjectID
	})
	if len(view.Candidates) > config.MaxIntegrationExecutionCandidates {
		view.Candidates = view.Candidates[:config.MaxIntegrationExecutionCandidates]
	}
	s.integration.mu.Lock()
	s.integration.executionTracker = view
	s.integration.executionTrackerConfig = cfg
	s.integration.mu.Unlock()
}

func (s *Server) stageTrackerIntegrationExecution() *IntegrationExecutionTracker {
	cfg := s.integrationConfig().Execution.Effective()
	if cfg.Mode == config.IntegrationOff {
		return nil
	}
	s.integration.mu.Lock()
	defer s.integration.mu.Unlock()
	if s.integration.executionTracker != nil && executionConfigEqual(cfg, s.integration.executionTrackerConfig) {
		view := *s.integration.executionTracker
		view.Statuses = append([]integration.ExecutionStatus(nil), view.Statuses...)
		view.Candidates = append([]IntegrationExecutionTrackerCandidate(nil), view.Candidates...)
		for index := range view.Candidates {
			view.Candidates[index].Members = append([]forgestate.IntegrationCandidateMember(nil), view.Candidates[index].Members...)
		}
		if view.Telemetry.RunnerMinutes != nil {
			minutes := *view.Telemetry.RunnerMinutes
			view.Telemetry.RunnerMinutes = &minutes
		}
		return &view
	}
	return &IntegrationExecutionTracker{Version: integration.ExecutionSchemaVersion, Mode: cfg.Mode,
		RuntimeVersion: versionHash, SourceVersion: "unknown", PublishedImage: "unknown",
		ObservationAgeSeconds: cfg.ObservationAge.Seconds(),
		Telemetry:             integration.Cost{Reason: "runtime cohort not yet observed; unknown is not zero"},
		Candidates:            []IntegrationExecutionTrackerCandidate{}}
}
