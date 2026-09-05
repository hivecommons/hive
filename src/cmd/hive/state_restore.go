package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
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
// pausedRestoreDetail builds the "restoring N paused agent(s)" fragment of
// the boot audit line. Two lessons from #4041 are baked in:
//
//   - On-demand agents (agent config on_demand, or on-demand per their pack
//     definition) are startup-paused BY DESIGN — brainstorm is triggered by
//     inception only and has never run at boot. Counting them alongside real
//     restored pauses inflated "restoring 9 paused agent(s)" on a hive whose
//     owner had paused 8, and that inflated bare count is what made a
//     deliberate quiesce read as a fresh systemic event on every upgrade
//     restart. They are reported separately, never in the headline count.
//   - The bare count says nothing about WHO paused WHAT. The per-trigger
//     breakdown ("dashboard-api: 8") lets a fleet owner reading logs tell an
//     owner-initiated mass pause from a login-detector cascade at a glance.
//
// statuses carries the manager's post-restore pause provenance; agents
// missing from it (not yet registered) count under the "unknown" trigger
// rather than being dropped — the total must always add up.
func pausedRestoreDetail(enabled map[string]config.AgentConfig, onDemandFromPack map[string]bool, statuses map[string]*agent.AgentProcess) string {
	restored := 0
	byDesign := 0
	byTrigger := map[string]int{}
	for name, ac := range enabled {
		if !ac.Paused {
			continue
		}
		if ac.OnDemand || onDemandFromPack[name] {
			byDesign++
			continue
		}
		restored++
		trigger := "unknown"
		if proc, ok := statuses[name]; ok && proc != nil && strings.TrimSpace(proc.PausedTrigger) != "" {
			trigger = proc.PausedTrigger
		}
		byTrigger[trigger]++
	}

	detail := fmt.Sprintf("restoring %d paused agent(s)", restored)
	if len(byTrigger) > 0 {
		triggers := make([]string, 0, len(byTrigger))
		for t := range byTrigger {
			triggers = append(triggers, t)
		}
		sort.Strings(triggers)
		parts := make([]string, 0, len(triggers))
		for _, t := range triggers {
			parts = append(parts, fmt.Sprintf("%s: %d", t, byTrigger[t]))
		}
		detail += " (" + strings.Join(parts, ", ") + ")"
	}
	if byDesign > 0 {
		detail += fmt.Sprintf("; %d on-demand agent(s) startup-paused by design", byDesign)
	}
	return detail
}

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
			// Replay the acting user too (#4041): without it, every restart
			// downgraded an owner-attributed pause to an anonymous one, and a
			// deliberate quiesce read like a malfunction days later.
			_ = agentMgr.PauseBy(name, trigger, reason, as.PausedBy)
			if as.PausedAt != nil {
				agentMgr.SeedPauseState(name, *as.PausedAt, trigger, reason, as.PausedBy)
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
		if as.RestartCount > 0 || len(as.RestartEvents) > 0 || as.LastRestartReason != "" {
			agentMgr.SeedRestartTelemetry(name, as.RestartCount, restartEventsFromSnapshot(as.RestartEvents), as.LastRestartReason)
		}
		// Without this the turn-loss measurement would reset on exactly the
		// event it exists to measure, and every spoke would permanently report
		// only what its CURRENT process lifetime lost — which for a fleet that
		// rolls often is close to nothing. See pkg/agent/turn_loss.go.
		if as.TurnLoss != nil {
			agentMgr.SeedTurnLoss(name, turnLossFromSnapshot(as.TurnLoss))
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

// turnLossFromSnapshot converts the persisted turn-loss record back into the
// manager's in-memory form. The inverse of turnLossToSnapshot in main.go.
func turnLossFromSnapshot(in *snapshot.AgentTurnLoss) agent.TurnLoss {
	if in == nil {
		return agent.TurnLoss{}
	}
	out := agent.TurnLoss{
		Interruptions: in.Interruptions,
		Producing:     in.Producing,
		UpperBound:    time.Duration(in.UpperBoundS * float64(time.Second)),
		Bytes:         in.Bytes,
	}
	for _, r := range in.Recent {
		rec := agent.TurnInterruption{
			At:        r.At,
			Reason:    r.Reason,
			SinceKick: time.Duration(r.SinceKickS * float64(time.Second)),
			Producing: r.Producing,
			Bytes:     r.Bytes,
		}
		if r.SinceOutputS != nil {
			d := time.Duration(*r.SinceOutputS * float64(time.Second))
			rec.SinceOutput = &d
		}
		out.Recent = append(out.Recent, rec)
	}
	return out
}
