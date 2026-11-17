package main

import (
	"errors"
	"log/slog"
	"maps"
	"sort"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/research"
)

// meetingsResearchBaseAgent is the configured agent whose backend and model
// the research agents reuse (hivecommons/hive#18294).
const meetingsResearchBaseAgent = "scanner"

// researchBaseName is the configured agent researchBase copies from, ""
// when there is none.
func researchBaseName(cfg *config.Config) string {
	if cfg == nil || len(cfg.Agents) == 0 {
		return ""
	}
	if _, ok := cfg.Agents[meetingsResearchBaseAgent]; ok {
		return meetingsResearchBaseAgent
	}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return names[0]
}

// researchStatusSource is the slice of *research.Pool the settings page
// reads.
type researchStatusSource interface {
	Status() research.Status
}

// meetingsResearchStatus reports the pool for Settings, Meeting attendee,
// or nil when d is not a pool.
func meetingsResearchStatus(cfg *config.Config, d live.ResearchDispatcher) dashboard.MeetingsResearchStatusFunc {
	src, ok := d.(researchStatusSource)
	if !ok {
		return nil
	}
	base := researchBase(cfg)
	name := researchBaseName(cfg)
	return func() dashboard.MeetingsResearchStatus {
		st := src.Status()
		return dashboard.MeetingsResearchStatus{Agents: st.Agents, Available: st.Available, BaseAgent: name, Backend: base.Backend, Model: base.Model}
	}
}

// researchBase picks the backend and model for research-1..N: the scanner's
// when it exists, else the first configured agent's by name. Only those two
// fields are copied; research.AgentConfig forces the read-only rest.
func researchBase(cfg *config.Config) config.AgentConfig {
	name := researchBaseName(cfg)
	if name == "" {
		return config.AgentConfig{}
	}
	a := cfg.Agents[name]
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
	base := researchBase(cfg)
	pool.Ensure(base)
	keepResearchAgents(pool.Agents(), research.AgentConfig(base))
	if logger != nil {
		logger.Info("[meetings] research pool ready", "agents", pool.Agents())
	}
	return pool
}

// researchAgents is the research pool's agent set once it is wired. The
// agents are runtime-only (not in hive.yaml), so every ReconcileAgents call
// (config reload, reviewer autoscale) must carry them, or it removes them as
// "no longer configured" and every meeting research request then fails with
// "no research agent available" (hivecommons/hive#18590 finding 6).
var researchAgents atomic.Pointer[map[string]config.AgentConfig]

// keepResearchAgents records the pool's agents for withResearchAgents.
func keepResearchAgents(names []string, cfg config.AgentConfig) {
	set := make(map[string]config.AgentConfig, len(names))
	for _, n := range names {
		set[n] = cfg
	}
	researchAgents.Store(&set)
}

// withResearchAgents returns configs plus the research pool's agents (when
// the pool is wired and a name is not already configured), for
// ReconcileAgents. configs is not modified.
func withResearchAgents(configs map[string]config.AgentConfig) map[string]config.AgentConfig {
	set := researchAgents.Load()
	if set == nil || len(*set) == 0 {
		return configs
	}
	out := maps.Clone(configs)
	if out == nil {
		out = make(map[string]config.AgentConfig, len(*set))
	}
	for n, c := range *set {
		if _, ok := out[n]; !ok {
			out[n] = c
		}
	}
	return out
}

// researchObserver is the slice of *research.Pool that reports progress.
type researchObserver interface {
	Observe(fn func(research.Event)) (remove func())
}

// researchSignal maps a pool event onto the live loop's signal. queued
// carries nothing the loop needs: it marks an item queued when it
// dispatches it. running tells it the kick reached its agent, which ends
// the wait of a request queued behind another question.
func researchSignal(ev research.Event) (live.ResearchSignal, bool) {
	sig := live.ResearchSignal{KickID: ev.KickID, Text: ev.Text}
	switch ev.Kind {
	case research.EventRunning:
		sig.Kind = live.SignalRunning
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
