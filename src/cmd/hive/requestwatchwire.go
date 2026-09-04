package main

import (
	"context"
	"errors"
	"log/slog"
)

import "github.com/hivecommons/hive/pkg/github/requestwatch"

func startRequestWatchers(ctx context.Context, watcher *requestwatch.Watcher, logger *slog.Logger) {
	if watcher == nil {
		return
	}
	go func() {
		if err := watcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("request watchers stopped", "error", err)
		}
	}()
}
