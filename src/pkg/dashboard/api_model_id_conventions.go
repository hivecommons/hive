package dashboard

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
)

// modelIDNormalizedAlertPrefix prefixes the per-agent warning raised when an
// agent's Claude model id was rewritten to its backend's naming convention
// (claude-sonnet-5.5 on claude, claude-sonnet-5-5 on copilot), either at
// config load or by a dashboard write.
const modelIDNormalizedAlertPrefix = "model-id-normalized-"

// modelIDConventionWarning is the operator-facing explanation of one rewrite.
func modelIDConventionWarning(agent, backend, from, to string) string {
	return fmt.Sprintf("Agent %s: model %q is not the %s spelling of this model and the %s CLI rejects it, so it was changed to %q.",
		agent, from, backend, backend, to)
}

// canonicalizeModelForBackend returns the model id to store for backend and,
// when it had to be rewritten, a warning for the operator. The rewrite is
// config.CanonicalizeModelForBackend, the one shared Claude/Copilot mapping:
// it only changes Claude ids on the claude and copilot backends, so gateway,
// non-Claude and other-backend ids are kept verbatim.
func canonicalizeModelForBackend(agent, backend, model string) (string, string) {
	to, changed := config.CanonicalizeModelForBackend(backend, model)
	if !changed {
		return model, ""
	}
	return to, modelIDConventionWarning(agent, backend, model, to)
}

// canonicalModelList returns backend's served model list with every Claude id
// in that backend's spelling, deduplicated, so the model picker can only ever
// offer an id the backend's CLI accepts.
func canonicalModelList(backend string, models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		canonical, _ := config.CanonicalizeModelForBackend(backend, m)
		out = append(out, canonical)
	}
	return dedupeModels(out)
}

// withModelWarning adds the model-id rewrite warning to a JSON response body
// so the dashboard can show it where the operator made the change.
func withModelWarning(resp map[string]any, warning string) map[string]any {
	if warning != "" {
		resp["warning"] = warning
	}
	return resp
}

// agentEffectiveBackendAndModel returns the backend and model name launches
// with: a live override wins over the saved config, as in launchInTmux.
func (s *Server) agentEffectiveBackendAndModel(name string) (string, string) {
	var backend, model string
	if ac, ok := s.configuredAgent(name); ok {
		backend, model = ac.Backend, ac.Model
	}
	if proc := s.runningAgentProcess(name); proc != nil {
		if proc.Config.Backend != "" {
			backend = proc.Config.Backend
		}
		if proc.Config.Model != "" {
			model = proc.Config.Model
		}
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}
		if proc.ModelOverride != "" {
			model = proc.ModelOverride
		}
	}
	return backend, model
}

// noteModelIDNormalization raises (or clears) the per-agent warning banner.
func (s *Server) noteModelIDNormalization(agent, warning string) {
	if warning == "" {
		s.ClearSystemAlert(modelIDNormalizedAlertPrefix + agent)
		return
	}
	s.logger.Warn("rewrote a Claude model id to the backend's naming convention", "agent", agent, "detail", warning)
	s.AddSystemAlert(modelIDNormalizedAlertPrefix+agent, "warning", warning)
}

// healCarriedModel fixes the model an agent carries over when it is switched
// onto a new backend. The backend switch changes only the backend, so an
// agent moving from copilot to claude keeps the dotted copilot id
// (claude-sonnet-5.5), which the Claude CLI rejects on every kick, and one
// moving from claude to copilot keeps the dashed claude id
// (claude-sonnet-5-5), which the Copilot CLI rejects. Returns the operator
// warning, or "" when nothing needed fixing.
func (s *Server) healCarriedModel(name, backend string) string {
	_, model := s.agentEffectiveBackendAndModel(name)
	to, warning := canonicalizeModelForBackend(name, backend, model)
	if warning == "" {
		return ""
	}
	if proc := s.runningAgentProcess(name); proc != nil {
		if err := s.deps.AgentMgr.SetModelOverride(name, to); err != nil {
			s.logger.Warn("failed to apply normalized model", "agent", name, "error", err)
		}
	}
	s.claimAgentFieldOwnership(name, to, "")
	return warning
}

// surfaceModelIDNormalizations handles each agent whose Claude model id the
// loader rewrote at boot (config.NormalizeModelIDConventions). It writes the
// fixed id to the agent's overlay file so the rewrite is durable: for managed
// agents the overlay replaces the hive.yaml entry on every load, so leaving
// the old spelling there would repeat the rewrite on each boot.
//
// A rewrite that was persisted needs nothing from the operator: the agent
// already runs on the corrected id, and the old spelling is often one the
// hive's own seed config wrote (hub-provisioned copilot agents carry the
// dashed id). Raising a banner for it on every boot or upgrade only produced
// undismissable noise (#19747), so it is logged instead. The banner is kept
// for a rewrite that could not be persisted, because that one repeats on
// every boot until the stored model is fixed.
func (s *Server) surfaceModelIDNormalizations() {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	agentsDir := s.deps.Config.Data.AgentsDir
	for _, n := range s.deps.Config.ModelIDNormalizations {
		warning := modelIDConventionWarning(n.Agent, n.Backend, n.From, n.To)
		ac, ok := s.deps.Config.Agents[n.Agent]
		if agentsDir == "" || !ok {
			s.AddSystemAlert(modelIDNormalizedAlertPrefix+n.Agent, "warning", warning)
			continue
		}
		if err := config.SaveAgentFile(agentsDir, n.Agent, ac); err != nil {
			s.logger.Error("failed to persist normalized model to agent overlay", "agent", n.Agent, "error", err)
			s.AddSystemAlert(modelIDNormalizedAlertPrefix+n.Agent, "warning", warning)
			continue
		}
		s.logger.Info("persisted normalized model id to agent overlay", "agent", n.Agent, "from", n.From, "to", n.To)
	}
}
