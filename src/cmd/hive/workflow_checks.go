package main

import (
	"context"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/checksfile"
)

// workflowEmitChecks is the dashboard's WorkflowEmitChecks dependency
// (hivecommons/hive#18355): binding a model that declares checks proposes
// .hive/workflows/checks.yml to the repository through the checks PR, as a
// GitHub-signed commit on a hive-owned branch, never a push to the base
// branch. The live client is resolved on every call so emits follow App
// credential rebuilds (#9621).
func workflowEmitChecks(cfg *config.Config, client func() *github.Client) func(ctx context.Context, repo string, m *workflowmodel.Model) (checksfile.Result, error) {
	return func(ctx context.Context, repo string, m *workflowmodel.Model) (checksfile.Result, error) {
		var gh *github.Client
		if client != nil {
			gh = client()
		}
		if gh == nil {
			return checksfile.Result{}, errNoForgeClient
		}
		return checksfile.Emit(ctx, gh, config.QualifyRepo(cfg.Project.Org, repo), "", m)
	}
}
