package dashboard

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/config"
)

// modelIDNormalizedAlertPrefix prefixes the per-agent warning raised when a
// claude agent's Copilot-style dotted model id was rewritten to the Claude
// CLI spelling, either at config load or by a dashboard write.
const modelIDNormalizedAlertPrefix = "model-id-normalized-"

// claudeModelIDWarning is the operator-facing explanation of one rewrite.
func claudeModelIDWarning(agent, from, to string) string {
	return fmt.Sprintf("Agent %s: model %q is the Copilot spelling and the Claude CLI rejects it, so it was changed to %q for the claude backend.",
		agent, from, to)
}

// canonicalizeModelForBackend returns the model id to store for backend and,
// when it had to be rewritten, a warning for the operator. Only the claude
// backend is normalized here: copilot already canonicalizes through
// crux.CanonicalizeCopilotModel, and gateway ids must be kept verbatim.
func canonicalizeModelForBackend(agent, backend, model string) (string, string) {
	if backend != config.ClaudeBackend {
		return model, ""
	}
	to, changed := config.CanonicalizeClaudeModel(model)
	if !changed {
		return model, ""
	}
	return to, claudeModelIDWarning(agent, model, to)
}

// canonicalClaudeModelList returns the claude backend's served model list
// with every id in the Claude CLI spelling, deduplicated, so the model picker
// can only ever offer an id the CLI accepts for this backend.
func canonicalClaudeModelList(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		canonical, _ := config.CanonicalizeClaudeModel(m)
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
	s.logger.Warn("rewrote a Copilot-style dotted Claude model id", "agent", agent, "detail", warning)
	s.AddSystemAlert(modelIDNormalizedAlertPrefix+agent, "warning", warning)
}

// healCarriedClaudeModel fixes the model an agent carries over when it is
// switched onto the claude backend. The backend switch changes only the
// backend, so an agent that ran on copilot keeps its dotted copilot id
// (claude-sonnet-5.5), which the Claude CLI rejects on every kick. Returns
// the operator warning, or "" when nothing needed fixing.
func (s *Server) healCarriedClaudeModel(name, backend string) string {
	if backend != config.ClaudeBackend {
		return ""
	}
	_, model := s.agentEffectiveBackendAndModel(name)
	to, warning := canonicalizeModelForBackend(name, backend, model)
	if warning == "" {
		return ""
	}
	if proc := s.runningAgentProcess(name); proc != nil {
		if err := s.deps.AgentMgr.SetModelOverride(name, to); err != nil {
			s.logger.Warn("failed to apply normalized claude model", "agent", name, "error", err)
		}
	}
	s.claimAgentFieldOwnership(name, to, "")
	return warning
}

// surfaceModelIDNormalizations raises one warning per agent whose dotted
// Claude model id the loader rewrote at boot (config.NormalizeClaudeModelIDs),
// and writes the fixed id to the agent's overlay file so the rewrite is
// durable: for managed agents the overlay replaces the hive.yaml entry on
// every load, so leaving it dotted would repeat the rewrite on each boot.
func (s *Server) surfaceModelIDNormalizations() {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	agentsDir := s.deps.Config.Data.AgentsDir
	for _, n := range s.deps.Config.ModelIDNormalizations {
		s.AddSystemAlert(modelIDNormalizedAlertPrefix+n.Agent, "warning", claudeModelIDWarning(n.Agent, n.From, n.To))
		ac, ok := s.deps.Config.Agents[n.Agent]
		if agentsDir == "" || !ok {
			continue
		}
		if err := config.SaveAgentFile(agentsDir, n.Agent, ac); err != nil {
			s.logger.Error("failed to persist normalized claude model to agent overlay", "agent", n.Agent, "error", err)
		}
	}
}
