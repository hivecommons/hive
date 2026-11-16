package main

import (
	"errors"
	"log/slog"
	"sort"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/research"
)

// meetingsResearchBaseAgent is the configured agent whose backend and model
// the research agents reuse (hivecommons/hive#18294).
const meetingsResearchBaseAgent = "scanner"

// researchBase picks the backend and model for research-1..N: the scanner's
// when it exists, else the first configured agent's by name. Only those two
// fields are copied; research.AgentConfig forces the read-only rest.
func researchBase(cfg *config.Config) config.AgentConfig {
	if cfg == nil || len(cfg.Agents) == 0 {
		return config.AgentConfig{}
	}
	a, ok := cfg.Agents[meetingsResearchBaseAgent]
	if !ok {
		names := make([]string, 0, len(cfg.Agents))
		for n := range cfg.Agents {
			names = append(names, n)
		}
		sort.Strings(names)
		a = cfg.Agents[names[0]]
	}
	return config.AgentConfig{Backend: a.Backend, Model: a.Model}
}

// wireMeetingsResearch builds the research agent pool when meetings are on
// and registers research-1..N with the runtime (not hive.yaml). A runtime
// without queued kicks (CRUX_MODE=sidecar) leaves research off: research
// intents then fail visibly in the meeting panel. It returns nil in that
// case, never a typed nil.
func wireMeetingsResearch(cfg *config.Config, rt any, logger *slog.Logger) live.ResearchDispatcher {
	if cfg == nil || !cfg.Meetings.IsEnabled() || rt == nil {
		return nil
	}
	pool, err := research.NewPool(rt, cfg.Meetings.ResearchPoolSizeOrDefault())
	if err != nil {
		if logger != nil {
			if errors.Is(err, research.ErrUnsupportedRuntime) {
				logger.Warn("[meetings] research pool unavailable on this runtime (CRUX_MODE=sidecar); meeting research is off", "error", err)
			} else {
				logger.Error("[meetings] research pool not created; meeting research is off", "error", err)
			}
		}
		return nil
	}
	pool.Ensure(researchBase(cfg))
	if logger != nil {
		logger.Info("[meetings] research pool ready", "agents", pool.Agents())
	}
	return pool
}

// researchObserver is the slice of *research.Pool that reports progress.
type researchObserver interface {
	Observe(fn func(research.Event)) (remove func())
}

// researchSignal maps a pool event onto the live loop's signal. queued and
// running carry nothing the loop needs: it marks an item queued when it
// dispatches it and running on its first finding.
func researchSignal(ev research.Event) (live.ResearchSignal, bool) {
	sig := live.ResearchSignal{KickID: ev.KickID, Text: ev.Text}
	switch ev.Kind {
	case research.EventFinding:
		sig.Kind = live.SignalFinding
	case research.EventDone:
		sig.Kind = live.SignalComplete
	case research.EventFailed:
		sig.Kind = live.SignalError
	default:
		return live.ResearchSignal{}, false
	}
	return sig, true
}

// routeResearch delivers research progress for meeting kicks to the live
// loop and returns the observer's remove func.
func routeResearch(d live.ResearchDispatcher, lr *live.Runner) func() {
	obs, ok := d.(researchObserver)
	if !ok || lr == nil {
		return func() {}
	}
	return obs.Observe(func(ev research.Event) {
		if sig, ok := researchSignal(ev); ok {
			lr.DeliverResearch(sig)
		}
	})
}
