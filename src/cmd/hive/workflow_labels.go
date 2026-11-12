package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// workflowProvisionLabels is the dashboard's WorkflowProvisionLabels
// dependency (hivecommons/hive#17620): when an owner turns workflow.enabled
// on from Settings → Workflows, every bound repository's model labels are
// provisioned at once through the sweep's own memoised path
// (automerge.Engine.EnsureStageLabels over the process-shared stage engine
// options and tracker), rather than on the next PR that needs a label.
// Without a GitHub client the call reports an error and the handler
// surfaces it as a warning; the flag itself still flips.
func workflowProvisionLabels(cfg *config.Config, client func() *github.Client, logger *slog.Logger) func(ctx context.Context, repo string) error {
	return func(ctx context.Context, repo string) error {
		var gh *github.Client
		if client != nil {
			gh = client()
		}
		if gh == nil {
			return errNoForgeClient
		}
		owner, name, ok := strings.Cut(config.QualifyRepo(cfg.Project.Org, repo), "/")
		if !ok || owner == "" || name == "" {
			return fmt.Errorf("repository %q is not owner/name", repo)
		}
		eng := automerge.New(gh, automerge.Options{
			Logger:           logger,
			StageLabelPolicy: stageLabelPolicyFunc(cfg),
			StageEngine:      stageEngineOptions(cfg, logger),
		})
		return eng.EnsureStageLabels(ctx, owner, name)
	}
}
