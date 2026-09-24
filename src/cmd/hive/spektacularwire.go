package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/spektacular"
)

// wireSpektacularRunner installs the Spektacular stage runner on the
// dashboard server when runs.spektacular.enabled is set
// (hivecommons/hive#8303). It is the one place the two worlds meet:
// pkg/dashboard exposes its lease registry as primitives and never imports
// pkg/spektacular (its internal-import ratchet), and pkg/spektacular takes
// that registry through an interface. It reports whether a runner was
// installed; with the feature off (the default) nothing is constructed and
// no Spektacular process is ever started.
func wireSpektacularRunner(cfg *config.Config, srv *dashboard.Server, logger *slog.Logger) bool {
	if cfg == nil || srv == nil || !cfg.Runs.Spektacular.Enabled {
		return false
	}
	binary := cfg.Runs.Spektacular.BinaryOrDefault()
	probe, err := spektacular.Probe(context.Background(), binary)
	srv.SetSpektacularStatus(dashboard.FrontendSpektacular{Present: probe.Present, Version: probe.Version, Binary: probe.Binary})
	if logger != nil {
		if err != nil {
			logger.Warn("[spektacular] binary not available", "binary", binary, "error", err)
		} else {
			logger.Info("[spektacular] binary detected", "binary", probe.Binary, "version", probe.Version)
		}
	}
	srv.SetStageRunner(spektacular.NewHubRunner(cfg.Runs, srv, logger))
	if logger != nil {
		logger.Info("[spektacular] stage runner installed",
			"binary", binary,
			"poll", cfg.Runs.Spektacular.PollInterval().String(),
			"max_stage_retries", cfg.Runs.MaxStageRetriesOrDefault())
	}
	return true
}
