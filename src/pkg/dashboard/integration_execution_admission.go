package dashboard

import (
	"context"
	"fmt"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
)

func (s *Server) integrationExecutionNativeAdmission(ctx context.Context, repo string, pr *gh.PullRequest) (string, error) {
	cfg := s.integrationConfig().Execution.Effective()
	if cfg.Mode != config.IntegrationEnforce || pr == nil {
		return "", nil
	}
	scope, enrolled := executionEnrolledScope(cfg, repo, pr.GetBase().GetRef())
	if !enrolled || scope.Authority != "native" {
		return "", nil
	}
	if err := cfg.Validate(); err != nil {
		return "execution native admission configuration unavailable", err
	}
	if s.deps == nil || s.deps.GHClient == nil {
		return "execution native forge authority client unavailable", nil
	}
	now := time.Now()
	owned, err := s.integrationExecutionStore()
	if err != nil {
		return "execution native actual workflow accounting unavailable", err
	}
	authority, err := s.deps.GHClient.ReadIntegrationExecutionAuthority(ctx, scope, s.integration.executionIdentity.ID)
	if err != nil || !authority {
		return "execution native designated executor unverified; observer-only, no fallback/takeover", err
	}
	s.nativeQueueTracker.mu.Lock()
	queued, known := s.nativeQueueTracker.observations[nativeQueueScopeKey(scope.Repo, scope.Base)]
	s.nativeQueueTracker.mu.Unlock()
	if !known || queued.Stale || !queued.EntriesComplete || now.Sub(queued.ObservedAt) > cfg.ObservationAge {
		return "execution native exact queue cohort unknown/stale; admission growth frozen", nil
	}
	if len(queued.Entries) >= cfg.MaxActive {
		return "execution native bounded Hive enqueue cohort is full; no queue jumping", nil
	}
	accounted := s.integrationExecutionCost(cfg, owned, now)
	if !accounted.Complete || accounted.RunnerMinutes == nil {
		return "execution native actual workflow cohort incomplete; unknown is not zero", nil
	}
	if accounted.Running+scope.ExpectedJobs > cfg.MaxJobs ||
		*accounted.RunnerMinutes+scope.ExpectedMinutes > cfg.MaxRunnerMinutes {
		return "execution native static Hive enqueue budget exhausted; no GitHub group/build-window control", nil
	}
	return "", nil
}

func (s *Server) integrationExecutionCost(cfg config.IntegrationExecutionConfig, store *integration.Store, now time.Time) integration.Cost {
	legacy, err := s.integrationStore()
	if err != nil {
		return integration.Cost{Reason: "existing authoritative workflow accounting unavailable: " + err.Error()}
	}
	total := 0.0
	out := integration.Cost{Complete: true, RunnerMinutes: &total, Reason: "actual author/native and owned integration workflow cohorts"}
	oldSnapshot, ownSnapshot := legacy.Snapshot(now), store.Snapshot(now)
	coverage := oldSnapshot.Coverage
	for _, scope := range cfg.Scopes {
		through := time.Time{}
		for _, observed := range coverage {
			if observed.Repo == scope.Repo && observed.Base == scope.Base {
				through = observed.Through
			}
		}
		if through.IsZero() || through.After(now) || now.Sub(through) > cfg.ObservationAge {
			out.Complete = false
			out.Reason = fmt.Sprintf("%s#%s author/native workflow cohort stale/unknown", scope.Repo, scope.Base)
			through = now
		}
		cohorts := []integration.Coverage{}
		for _, sources := range [][]integration.Coverage{oldSnapshot.Coverage, ownSnapshot.Coverage} {
			found := false
			for _, observed := range sources {
				if observed.Repo == scope.Repo && observed.Base == scope.Base {
					cohorts = append(cohorts, observed)
					found = true
				}
			}
			if !found {
				cohorts = append(cohorts, integration.Coverage{})
			}
		}
		accounted := integration.ExecutionCohortCost(scope.Repo, scope.Base, through.Add(-cfg.Window), through,
			cohorts, oldSnapshot.Jobs, ownSnapshot.Jobs)
		if !accounted.Complete || accounted.RunnerMinutes == nil {
			out.Complete = false
			out.Reason = fmt.Sprintf("%s#%s authoritative job coverage incomplete; growth frozen", scope.Repo, scope.Base)
		} else {
			total += *accounted.RunnerMinutes
		}
		out.Jobs += accounted.Jobs
		out.Running += accounted.Running
		out.ObservedRunnerMinutes += accounted.ObservedRunnerMinutes
	}
	if !out.Complete {
		out.RunnerMinutes = nil
	}
	return out
}
