package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/proxy"
)

// bootWatchersDeps are the long-lived effects bootWatchers performs (#7571,
// step 2). The phase's substance is the hive.yaml reload callback; a test
// captures it through newConfigWatcher and drives it directly, so the
// preserve/swap/re-sync logic runs without a filesystem watcher.
type bootWatchersDeps struct {
	// newConfigWatcher is config.NewWatcher. The fake records onReload and
	// still returns a real (unstarted) watcher so SkipNext keeps working.
	newConfigWatcher func(path string, onReload func(*config.Config), logger *slog.Logger) *config.Watcher
	// startConfigWatcher is `go w.Start(ctx)`.
	startConfigWatcher func(ctx context.Context, w *config.Watcher)
	// startAgent is the goroutine that launches an agent a reload added.
	startAgent func(ctx context.Context, mgr *agent.Manager, name string, logger *slog.Logger)
	// refreshAgentTokens is `go mgr.RefreshAgentTokens(ctx)` after an App
	// auth rebuild (#4072).
	refreshAgentTokens func(ctx context.Context, mgr *agent.Manager)
	// registerGitHubHost is proxy.RegisterGitHubHost (GHE allowlisting).
	registerGitHubHost func(host string)
}

func defaultBootWatchersDeps() bootWatchersDeps {
	return bootWatchersDeps{
		newConfigWatcher:   config.NewWatcher,
		startConfigWatcher: func(ctx context.Context, w *config.Watcher) { go w.Start(ctx) },
		startAgent: func(ctx context.Context, mgr *agent.Manager, name string, logger *slog.Logger) {
			go func() {
				logger.Info("audit: starting reconciled agent", "name", name, "trigger", "config-reload")
				if err := mgr.Start(ctx, name); err != nil {
					logger.Warn("failed to start reconciled agent", "name", name, "error", err)
				}
			}()
		},
		refreshAgentTokens: func(ctx context.Context, mgr *agent.Manager) { go mgr.RefreshAgentTokens(ctx) },
		registerGitHubHost: proxy.RegisterGitHubHost,
	}
}
