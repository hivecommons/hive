package spektacular

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planengine"
)

// HubRunner is the boot-time shape of the stage observer: it satisfies the
// dashboard's StageRunner interface (Tick without a result) so the contribute
// hub's cleanup loop can drive it without knowing this package.
type HubRunner struct {
	runner *planengine.Runner
}

// NewHubRunner builds the production observer from config: the Spektacular
// engine over the configured binary, driving the dashboard's lease registry.
func NewHubRunner(cfg config.RunsConfig, reg LeaseRegistry, logger *slog.Logger) *HubRunner {
	engine := NewBinaryEngine(cfg.Spektacular.BinaryOrDefault())
	return &HubRunner{runner: &planengine.Runner{
		Engine:   engine,
		Poll:     cfg.Spektacular.PollInterval(),
		Registry: planengine.NewLeaseRegistryAdapter(reg, engine),
		Logger:   logger,
	}}
}

// Tick runs one poll; the result is logged by the observer itself.
func (h *HubRunner) Tick(ctx context.Context, now time.Time) {
	if h == nil || h.runner == nil {
		return
	}
	h.runner.Tick(ctx, now)
}

// Runner exposes the underlying poll loop (tests and diagnostics).
func (h *HubRunner) Runner() *planengine.Runner {
	if h == nil {
		return nil
	}
	return h.runner
}
