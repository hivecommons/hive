package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
)

func (s *Server) reconcileIntegrationMerges(ctx context.Context, scope config.IntegrationScope, from, through time.Time, coverage *integration.Coverage) error {
	parts := strings.SplitN(scope.Repo, "/", 2)
	list, response, err := s.deps.GHClient.GoGitHub().PullRequests.List(ctx, parts[0], parts[1], &gh.PullRequestListOptions{
		State: "closed", Base: scope.Base, Sort: "updated", Direction: "desc", ListOptions: gh.ListOptions{PerPage: integrationReconcilePageSize}})
	if err != nil {
		coverage.MergedComplete, coverage.MergeReason = false, "merged PR receipt listing unavailable"
		s.webhookLogger().Warn("integration merged-PR accounting unavailable", "repo", scope.Repo, "error", err)
		return nil
	}
	coverage.MergedComplete = response == nil || response.NextPage == 0
	for _, pr := range list {
		if pr == nil {
			coverage.MergedComplete = false
			coverage.MergeReason = "missing closed PR observation"
			continue
		}
		if !pr.GetUpdatedAt().Time.IsZero() && pr.GetUpdatedAt().Time.Before(from) {
			coverage.MergedComplete = true
			break
		}
		if pr.GetMergedAt().Time.IsZero() || pr.GetMergedAt().Time.Before(from) || pr.GetMergedAt().Time.After(through) {
			continue
		}
		if pr.GetBase().GetRef() != scope.Base {
			coverage.MergedComplete = false
			coverage.MergeReason = "merged PR base attribution unavailable"
			continue
		}
		receipt := integration.IntegrationReceipt{Repo: scope.Repo, Base: scope.Base, PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), MergeSHA: pr.GetMergeCommitSHA(),
			MergedAt: pr.GetMergedAt().Time, Source: "github-merged-pr-rest-v1"}
		store, storeErr := s.integrationStore()
		if storeErr != nil {
			return storeErr
		}
		id := fmt.Sprintf("merged-rest/%s/%d/%s", scope.Repo, pr.GetNumber(), receipt.MergeSHA)
		if store.HasEvent(id) {
			continue
		}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: id, At: through, Integrated: &receipt}); err != nil {
			return err
		}
	}
	if !coverage.MergedComplete && coverage.MergeReason == "" {
		coverage.MergeReason = "bounded merged-PR cohort overflow"
	}
	return nil
}
