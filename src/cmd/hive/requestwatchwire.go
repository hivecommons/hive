package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hivecommons/hive/pkg/github/requestwatch"
)

// startRequestWatchers runs the PR and issue request watchers until ctx is
// cancelled. The returned channel closes once both loops have exited (see
// requestwatch.Watcher.Run), or immediately for a nil watcher.
func startRequestWatchers(ctx context.Context, watcher *requestwatch.Watcher, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if watcher == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		if err := watcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("request watchers stopped", "error", err)
		}
	}()
	return done
}
