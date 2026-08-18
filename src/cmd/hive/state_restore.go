package main

import (
	"log/slog"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/snapshot"
)

// restoreAgentRuntimeState replays the per-agent runtime state persisted in
// /data/hive-state.json — pause state, CLI/model pins, model/backend
// overrides, restart counts, kick history, and the dashboard-editable config
// fields — onto the freshly-built agent manager at boot. Factored out of
// main() so the replay is testable (#3961: overrides silently failed to
// survive a pod restart and nothing could catch it).
//
// Ordering contract: the manager's gateway-name predicate
// (SetGatewayBackendChecker) MUST already be wired when this runs.
// SetBackendOverride validates backend names against it, so replaying a saved
// gateway-named backend override before the predicate exists gets rejected —
// which is exactly the pre-#3961 boot order. The rejection was also silently
// swallowed here while "backend override restored" was logged unconditionally,
// so the operator saw a successful restore of a value that was already gone.
// Failures are now logged as the failures they are.
func restoreAgentRuntimeState(saved *snapshot.PersistedState, cfg *config.Config, agentMgr *agent.Manager, logger *slog.Logger) {
	for name, as := range saved.Agents {
		if _, inConfig := cfg.Agents[name]; !inConfig {
			logger.Info("skipping saved state for agent not in config", "agent", name)
			continue
		}
		if as.Paused {
			reason := as.PausedReason
			if reason == "" {
				reason = "persisted pause state"
			}
			trigger := as.PausedTrigger
			if trigger == "" {
				trigger = "state-restore"
			}
			_ = agentMgr.Pause(name, trigger, reason)
			if as.PausedAt != nil {
				agentMgr.SeedPauseState(name, *as.PausedAt, trigger, reason)
			}
		}
		if as.PinnedCLI != "" {
			_ = agentMgr.PinCLI(name, as.PinnedCLI)
		}
		if as.PinnedModel != "" {
			_ = agentMgr.PinModel(name, as.PinnedModel)
		}
		if as.ModelOverride != "" {
			if err := agentMgr.SetModelOverride(name, as.ModelOverride); err != nil {
				logger.Warn("model override NOT restored", "agent", name, "model", as.ModelOverride, "error", err)
			} else {
				logger.Info("model override restored", "agent", name, "model", as.ModelOverride)
			}
		}
		if as.BackendOverride != "" {
			if err := agentMgr.SetBackendOverride(name, as.BackendOverride); err != nil {
				// The one legitimate path here is a gateway that was deleted
				// from the config after the override was saved. Anything else
				// is a wiring-order regression (see the doc comment above).
				logger.Warn("backend override NOT restored — agent will run on its config backend",
					"agent", name, "backend", as.BackendOverride, "error", err)
			} else {
				logger.Info("backend override restored", "agent", name, "backend", as.BackendOverride)
			}
		}
		if as.RestartCount > 0 {
			agentMgr.SeedRestartCount(name, as.RestartCount)
		}
		if as.LastKick != nil {
			agentMgr.SeedLastKick(name, *as.LastKick)
		}
		if len(as.KickHistory) > 0 {
			records := make([]agent.KickRecord, len(as.KickHistory))
			for i, ke := range as.KickHistory {
				records[i] = agent.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent, Snippet: ke.Snippet}
			}
			agentMgr.SeedKickHistory(name, records)
		}
		if agentCfg, ok := cfg.Agents[name]; ok {
			if as.DisplayName != "" && agentCfg.DisplayName == "" {
				agentCfg.DisplayName = as.DisplayName
			}
			if as.Description != "" && agentCfg.Description == "" {
				agentCfg.Description = as.Description
			}
			if as.Enabled != nil {
				agentCfg.Enabled = *as.Enabled
			}
			if as.ClearOnKick != nil {
				agentCfg.ClearOnKick = *as.ClearOnKick
			}
			if as.StaleTimeout != nil {
				agentCfg.StaleTimeout = *as.StaleTimeout
			}
			if as.RestartStrategy != "" {
				agentCfg.RestartStrategy = as.RestartStrategy
			}
			cfg.Agents[name] = agentCfg
			_ = agentMgr.UpdateConfig(name, agentCfg)
		}
	}
}
