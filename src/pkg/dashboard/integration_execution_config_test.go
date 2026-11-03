package dashboard

import (
	"context"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"testing"
	"time"
)

func TestExecutionDefaultOffNeverInitializesWriterAndTrackerUnknownIsNotGreen(t *testing.T) {
	s := newTestServer()
	if err := s.ReconcileIntegrationExecution(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.stageTrackerIntegrationExecution() != nil || s.integration.executionStore != nil || s.integration.executionIdentity.ID != "" {
		t.Fatal("OFF opened execution identity/ledger or changed tracker")
	}
	s.deps = &Dependencies{Config: &config.Config{IntegrationOrchestration: config.IntegrationOrchestrationConfig{
		Execution: config.IntegrationExecutionConfig{Mode: config.IntegrationAdvisory, ObservationAge: time.Minute}}}}
	view := s.stageTrackerIntegrationExecution()
	if view == nil || view.Mode != "advisory" || view.Telemetry.Complete || view.Telemetry.RunnerMinutes != nil || view.SourceVersion != "unknown" || view.PublishedImage != "unknown" {
		t.Fatalf("unobserved runtime became green: %+v", view)
	}
}

func TestExecutionProfileCASAndScopedReleaseSelection(t *testing.T) {
	parent := config.IntegrationOrchestrationConfig{Mode: config.IntegrationAdvisory}
	onlyExecution := parent
	onlyExecution.Execution.Mode = config.IntegrationAdvisory
	if executionParentChanged(parent, onlyExecution) {
		t.Fatal("execution-only update would release authoring reservations")
	}
	if !executionConfigEqual(config.IntegrationExecutionConfig{}, config.IntegrationExecutionConfig{Mode: config.IntegrationOff}) {
		t.Fatal("zero OFF changed identity")
	}
	before := config.IntegrationExecutionConfig{Mode: config.IntegrationEnforce, Scopes: []config.IntegrationExecutionScope{
		{Repo: "o/r", Base: "v5", Authority: "hive", AuthorityCommit: "old"}, {Repo: "o/other", Base: "v5", Authority: "native"}}}
	after := before
	after.Scopes = append([]config.IntegrationExecutionScope(nil), before.Scopes...)
	if !executionConfigEqual(before, after) || len(executionReleasedScopes(before, after)) != 0 {
		t.Fatal("unchanged profile released reservations")
	}
	after.Scopes[0].AuthorityCommit = "new"
	if executionConfigEqual(before, after) {
		t.Fatal("CAS ignored authority epoch")
	}
	released := executionReleasedScopes(before, after)
	if len(released) != 1 || released[0].Repo != "o/r" {
		t.Fatalf("release escaped own changed scope: %+v", released)
	}
	after.Mode = config.IntegrationOff
	if len(executionReleasedScopes(before, after)) != 2 {
		t.Fatal("rollback retained new-layer reservations")
	}
	s := newTestServer()
	if err := s.executionHandoverGuard(context.Background(), before, after); err == nil {
		t.Fatal("missing forge reader became proven revocation")
	}
}

func TestExecutionAccountingCacheRequiresActualCompleteJobsAndIsBounded(t *testing.T) {
	s := newTestServer()
	now := time.Now()
	s.rememberExecutionCost("o/r", ghpkg.IntegrationCandidateObservation{RunID: 1, Attempt: 1, JobCompleteness: "unknown"}, now)
	if len(s.integration.executionCostObservations) != 0 {
		t.Fatal("unknown cohort became zero-cost complete")
	}
	for index := 1; index <= integrationExecutionMaxCostCache+1; index++ {
		s.rememberExecutionCost("o/r", ghpkg.IntegrationCandidateObservation{RunID: int64(index), Attempt: 1, JobCompleteness: "complete",
			Jobs: []forgestate.IntegrationRunnerJob{{RunID: int64(index), Attempt: 1, JobID: int64(index), Status: "completed"}}}, now)
	}
	if len(s.integration.executionCostObservations) > integrationExecutionMaxCostCache {
		t.Fatal("accounting cache grew without bound")
	}
}
