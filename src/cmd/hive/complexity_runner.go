package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	"github.com/hivecommons/hive/pkg/github"
)

// complexityRunnerShutdownHook names the pre-shutdown hook that waits for an
// in-flight snapshot pass to finish.
const complexityRunnerShutdownHook = "stop-complexity-runner"

// complexityRepos lists the managed repos as "org/name" from the live config,
// so a repo added or paused after boot is picked up on the next pass (#12363).
func complexityRepos(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	repos := cfg.ActiveRepos()
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		out = append(out, fullRepoName(repo, cfg.Project.Org))
	}
	return out
}

// newComplexityRunner builds the monthly complexity-snapshot runner (RFC
// #12062, #12363) or returns nil when evidence or the complexity leaf is off.
// client is read on every fetch so an App credential rebuild (or a hosted
// spoke receiving its App over the heartbeat after boot) is followed.
func newComplexityRunner(cfg *config.Config, client func() *github.Client, logger *slog.Logger) *complexity.Runner {
	if cfg == nil || !cfg.Evidence.IsEnabled() || !cfg.Evidence.Complexity.IsEnabled() {
		return nil
	}
	r := &complexity.Runner{
		Root:   github.ReviewEvidenceRoot,
		Repos:  func() []string { return complexityRepos(cfg) },
		Logger: logger,
		Source: complexity.SourceFunc(func(ctx context.Context, repo string) (complexity.Checkout, error) {
			return client().FetchDefaultBranchTree(ctx, repo)
		}),
	}
	if cfg.Evidence.Complexity.StaticAnalysis {
		r.Analyzers = complexity.DefaultAnalyzers()
	}
	return r
}

// startComplexityRunner starts the runner on the process context and
// registers a pre-shutdown hook that waits for the loop to exit so a pass
// mid-write is not cut off. A nil runner is a no-op.
func (b *boot) startComplexityRunner(r *complexity.Runner) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithCancel(b.ctx)
	done := r.Start(ctx)
	b.preShutdownHooks.add(complexityRunnerShutdownHook, func() {
		cancel()
		<-done
	})
}
