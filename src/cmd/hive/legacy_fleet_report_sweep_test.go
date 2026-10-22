package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestHiveWatchesRepo(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Repos = []string{"acme/widgets", " HiveCommons/Hive "}
	if !hiveWatchesRepo(cfg, "hivecommons/hive") || !hiveWatchesRepo(cfg, "acme/widgets") {
		t.Fatal("watched repo not recognised")
	}
	if hiveWatchesRepo(cfg, "acme/gadgets") || hiveWatchesRepo(nil, "hivecommons/hive") {
		t.Fatal("unwatched repo (or nil config) matched")
	}
}

// A hive that does not watch the report repo never runs the sweep, so it
// does not even stamp lastRun; one that does but has no client is a no-op.
func TestRunLegacyFleetReportSweepIfDueGates(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Repos = []string{"acme/widgets"}
	var last time.Time
	runLegacyFleetReportSweepIfDue(context.Background(), nil, cfg, nil, &last, slog.Default())
	if !last.IsZero() {
		t.Fatal("sweep ran without a client")
	}
	runLegacyFleetReportSweepIfDue(context.Background(), nil, nil, nil, &last, slog.Default())
	if !last.IsZero() {
		t.Fatal("sweep ran without a config")
	}
}
