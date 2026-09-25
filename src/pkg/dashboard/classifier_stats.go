package dashboard

import (
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
)

const defaultJevDashboardAPIKeyEnv = "JEV_API_KEY"

func (s *Server) handleClassifierStats(w http.ResponseWriter, r *http.Request) {
	stats := classify.CurrentStats()
	openRouterConnected := strings.TrimSpace(s.resolveOpenRouterKey()) != ""
	keySource := "none"
	if s.classifierEnvKeyConfigured() {
		keySource = "env"
	} else if openRouterConnected {
		keySource = "openrouter"
	}
	jsonResponse(w, map[string]interface{}{
		"decisions":               stats.Decisions,
		"estimated_input_tokens":  stats.EstimatedInputTokens,
		"estimated_spend_usd":     stats.EstimatedSpendUSD,
		"openrouter_connected":    openRouterConnected,
		"key_source":              keySource,
		"jev_api_key_env_present": keySource == "env",
	})
}

func (s *Server) classifierEnvKeyConfigured() bool {
	envName := ""
	if s.deps != nil && s.deps.Config != nil {
		envName = strings.TrimSpace(s.deps.Config.Classifier.EffectiveJev().APIKeyEnv)
	}
	if envName != "" && strings.TrimSpace(os.Getenv(envName)) != "" {
		return true
	}
	return strings.TrimSpace(os.Getenv(defaultJevDashboardAPIKeyEnv)) != ""
}

func (s *Server) handleGovernorClassifier(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Backend       *string  `json:"backend"`
		Mode          *string  `json:"mode"`
		MinConfidence *float64 `json:"minConfidence"`
		Decisions     []string `json:"decisions"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	cfg := s.deps.Config
	next := cfg.Classifier
	if body.Backend != nil {
		next.Backend = strings.ToLower(strings.TrimSpace(*body.Backend))
	}
	if body.Mode != nil {
		next.Mode = strings.ToLower(strings.TrimSpace(*body.Mode))
	}
	if body.MinConfidence != nil {
		next.Jev.MinConfidence = *body.MinConfidence
	}
	if body.Decisions != nil {
		next.Jev.Decisions = normalizedClassifierDecisions(body.Decisions)
	}
	if next.EffectiveBackend() == "jev" && strings.TrimSpace(next.Jev.APIKeyEnv) == "" &&
		strings.TrimSpace(os.Getenv(defaultJevDashboardAPIKeyEnv)) != "" {
		next.Jev.APIKeyEnv = defaultJevDashboardAPIKeyEnv
	}
	if err := next.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.Classifier = next
	if err := classify.ConfigureDecider(cfg); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveConfig(); err != nil {
		jsonError(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, classifierSectionResponse(cfg))
}

func normalizedClassifierDecisions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

func classifierSectionResponse(cfg *config.Config) map[string]interface{} {
	simple, complex := classify.TierKeywords()
	out := map[string]interface{}{
		"simpleKeywords": simple,
		"complexSignals": complex,
	}
	if cfg == nil {
		return out
	}
	jev := cfg.Classifier.EffectiveJev()
	out["backend"] = cfg.Classifier.EffectiveBackend()
	out["mode"] = cfg.Classifier.EffectiveMode()
	out["jev"] = map[string]interface{}{
		"provider":      jev.Provider,
		"model":         jev.Model,
		"endpoint":      jev.Endpoint,
		"apiKeyEnv":     jev.APIKeyEnv,
		"minConfidence": jev.MinConfidence,
		"timeout":       jev.Timeout.String(),
		"decisions":     jev.Decisions,
	}
	return out
}
