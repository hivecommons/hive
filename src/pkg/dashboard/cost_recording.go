package dashboard

import (
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// costRecordingStaleAfter is how long the running agents' configured models
// may go without any recorded usage growth before /api/cost flags live cost
// recording as stale (hivecommons/hive#13485).
const costRecordingStaleAfter = 24 * time.Hour

// costRecording is the additive `recording` block of /api/cost. It tells the
// Cost card whether the figures it shows actually include the models the
// running agents are configured to use, so a broken usage recorder cannot
// hide behind a confident run-rate.
type costRecording struct {
	// AgentModels are the (alias-resolved) models configured on running agents.
	AgentModels []string `json:"agent_models"`
	// UnrecordedModels is the subset of AgentModels with no recorded usage in
	// the current token summary.
	UnrecordedModels []string `json:"unrecorded_models"`
	// RecentModels are the models whose recorded usage grew within the stale
	// window, i.e. what the recent figures actually reflect.
	RecentModels []string `json:"recent_models"`
	// LastPricedGrowthAt is when a model with an exact list price last grew in
	// the persisted cost history (unix ms; 0 = never seen).
	LastPricedGrowthAt int64 `json:"last_priced_growth_at,omitempty"`
	// LastAgentModelGrowthAt is when any of AgentModels last grew (unix ms).
	LastAgentModelGrowthAt int64 `json:"last_agent_model_growth_at,omitempty"`
	// Stale is true when running agents' models have recorded nothing for
	// longer than StaleAfterMs while the history covers that long.
	Stale        bool  `json:"stale"`
	StaleAfterMs int64 `json:"stale_after_ms"`
}

// costRecordingModelKey normalizes a model id for matching agent config
// models against recorded usage rows. It delegates to the tokens normalizer
// so both sides agree ("anthropic/claude-sonnet-5.5" and "claude-sonnet-5-5"
// match).
func costRecordingModelKey(model string) string {
	return tokens.NormalizeModelID(model)
}

// costRecordingIsAlias reports whether a normalized key is a routing alias
// ("auto", "default") rather than a real model; it shares the tokens rule.
func costRecordingIsAlias(key string) bool {
	return tokens.IsRoutingAlias(key)
}

// costRecording builds the recording block from the current status, the
// live token summary and the persisted cost history.
func (s *Server) costRecording(est costEstimated) costRecording {
	var agents []FrontendAgent
	var byAgentModel map[string]string
	s.statusMu.RLock()
	if s.status != nil {
		agents = append(agents, s.status.Agents...)
		byAgentModel = s.status.Tokens.ByAgentModel
	}
	s.statusMu.RUnlock()
	return buildCostRecording(agents, byAgentModel, est.ByModel, s.CostHistory(), time.Now())
}

func buildCostRecording(agents []FrontendAgent, byAgentModel map[string]string, byModel []costModelEntry, history []CostHistoryEntry, now time.Time) costRecording {
	rec := costRecording{
		AgentModels:      []string{},
		UnrecordedModels: []string{},
		RecentModels:     []string{},
		StaleAfterMs:     costRecordingStaleAfter.Milliseconds(),
	}

	agentKeys := map[string]bool{}
	for _, a := range agents {
		if a.State != "running" || a.Paused || a.OffByCadence || a.OperatorStopped {
			continue
		}
		model := a.Model
		if costRecordingIsAlias(costRecordingModelKey(model)) {
			model = byAgentModel[a.Name]
		}
		key := costRecordingModelKey(model)
		if costRecordingIsAlias(key) || agentKeys[key] {
			continue
		}
		agentKeys[key] = true
		rec.AgentModels = append(rec.AgentModels, model)
	}
	sort.Strings(rec.AgentModels)

	recorded := map[string]bool{}
	for _, m := range byModel {
		if m.Input > 0 || m.Output > 0 || m.CacheRead > 0 || m.CacheCreate > 0 {
			recorded[costRecordingModelKey(m.Name)] = true
		}
	}
	for _, m := range rec.AgentModels {
		if !recorded[costRecordingModelKey(m)] {
			rec.UnrecordedModels = append(rec.UnrecordedModels, m)
		}
	}

	recentCutoff := now.Add(-costRecordingStaleAfter).UnixMilli()
	recent := map[string]bool{}
	var prev map[string]CostModelSnap
	for _, e := range history {
		if e.Models == nil {
			continue
		}
		if prev != nil {
			for name, snap := range e.Models {
				before, seen := prev[name]
				if snap.Input+snap.Output <= before.Input+before.Output && seen {
					continue
				}
				if !seen && snap.Input+snap.Output == 0 {
					continue
				}
				if _, priced := tokens.LookupPrice(name); priced && e.Timestamp > rec.LastPricedGrowthAt {
					rec.LastPricedGrowthAt = e.Timestamp
				}
				if agentKeys[costRecordingModelKey(name)] && e.Timestamp > rec.LastAgentModelGrowthAt {
					rec.LastAgentModelGrowthAt = e.Timestamp
				}
				if e.Timestamp >= recentCutoff {
					recent[name] = true
				}
			}
		}
		prev = e.Models
	}
	for name := range recent {
		rec.RecentModels = append(rec.RecentModels, name)
	}
	sort.Strings(rec.RecentModels)

	// Only judge staleness once the history itself spans the stale window, so
	// a fresh install does not raise a false alarm.
	historyCovers := len(history) > 0 && history[0].Timestamp <= recentCutoff
	if len(rec.AgentModels) > 0 && historyCovers && rec.LastAgentModelGrowthAt < recentCutoff {
		rec.Stale = true
	}
	return rec
}
