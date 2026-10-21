package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
)

// forgeStateReadyLogInterval is how often the startup gate reports that it
// is still waiting for the state cache's first reconcile, so a slow or
// budget-denied first list is visible in the log rather than a silent hang.
const forgeStateReadyLogInterval = time.Minute

// applyGitHubClientForgeState attaches the per-repo GitHub state cache
// (pkg/forgestate, hivecommons/hive#13375) to the client when
// `forgestate.enabled` is on. The store is built once, on the first client,
// and reconciles in the background for the life of the process; rebuilt
// clients re-attach the same store so a token rotation never empties the
// cache or restarts the startup gate. The store fetches through whichever
// client is live at the time, never the one it was built with.
func (b *boot) applyGitHubClientForgeState(client *github.Client) {
	if client == nil || b.cfg == nil || !b.cfg.ForgeState.Enabled {
		return
	}
	b.forgeStateOnce.Do(func() {
		opts := forgestate.OptionsFromConfig(b.cfg.ForgeState)
		opts.Repos = client.Repositories()
		opts.Fetcher = github.ForgeStateFetcher{Current: b.currentGitHubClient}
		opts.Logger = b.logger
		opts.Critical = func() bool {
			c := b.currentGitHubClient()
			if c == nil {
				return false
			}
			mode, _ := c.APIBudgetMode()
			return mode == github.APIBudgetCritical
		}
		store, err := forgestate.New(opts)
		if err != nil {
			b.logger.Error("forgestate: state cache disabled; governor lists GitHub each cycle", "error", err)
			return
		}
		b.forgeState.Store(store)
		// The sentinel's reconcile sweep enumerates open PRs from the
		// same cache instead of listing each repo (#13379).
		github.SetSentinelPRCache(store)
		ctx := b.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		go func() {
			if err := store.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				b.logger.Error("forgestate: reconcile loop stopped", "error", err)
			}
		}()
		b.logger.Info("forgestate: governor work source reads the state cache",
			slog.Int("repos", len(opts.Repos)),
			slog.Duration("reconcile_interval", opts.ReconcileInterval),
			slog.Duration("freshness_bound", opts.FreshnessBound))
	})
	client.SetForgeState(b.forgeState.Load())
}

// waitForgeStateReady is the startup gate (#13375): when the state cache is
// on, no decision pass runs before every repo has completed its first
// successful reconcile. Returns ctx.Err() when shutdown wins the wait; a
// nil store (cache off, or construction failed) returns at once.
func waitForgeStateReady(ctx context.Context, store *forgestate.Store, logger *slog.Logger, logEvery time.Duration) error {
	if store == nil {
		return nil
	}
	if logEvery <= 0 {
		logEvery = forgeStateReadyLogInterval
	}
	started := time.Now()
	for {
		waitCtx, cancel := context.WithTimeout(ctx, logEvery)
		err := store.WaitReady(waitCtx)
		cancel()
		if err == nil {
			if logger != nil {
				logger.Info("forgestate: first reconcile complete; governor decision passes may start",
					slog.Duration("waited", time.Since(started)))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if logger != nil {
			logger.Info("forgestate: waiting for the first reconcile before the first decision pass",
				slog.Duration("waited", time.Since(started)))
		}
	}
}
