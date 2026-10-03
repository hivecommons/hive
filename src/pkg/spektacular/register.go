package spektacular

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planengine"
)

// The CLI engine is the default planning engine (ADR-0021 AC-1): linking this
// package registers it under its receipt name.
func init() {
	planengine.Register(EngineName, buildEngine)
}

// buildEngine is the registered planengine.Builder. It builds the production
// engine over runs.spektacular's binary; the poll cadence
// (runs.spektacular.PollInterval) belongs to the observer, not the engine.
func buildEngine(cfg config.RunsConfig, _ *slog.Logger) (planengine.Engine, error) {
	return NewBinaryEngine(cfg.Spektacular.BinaryOrDefault()), nil
}
