package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"sort"
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
		"disagreements":           stats.Disagreements,
		"rule_suggestions":        stats.RuleSuggestions,
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
		MinConfidence *float64 `json:"minConfidence"`
		Decisions     []string `json:"decisions"`
		Suggestion    *struct {
			Decision string `json:"decision"`
			Answer   string `json:"answer"`
			Keyword  string `json:"keyword"`
		} `json:"suggestion"`
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
	next.Mode = ""
	if body.MinConfidence != nil {
		next.Jev.MinConfidence = *body.MinConfidence
	}
	if body.Decisions != nil {
		next.Jev.Decisions = normalizedClassifierDecisions(body.Decisions)
	}
	if body.Suggestion != nil {
		if err := applyClassifierSuggestion(cfg, body.Suggestion.Decision, body.Suggestion.Answer, body.Suggestion.Keyword); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		next = cfg.Classifier
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
	refreshClassifierRuntimeConfig(cfg)
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

func applyClassifierSuggestion(cfg *config.Config, decision, answer, keyword string) error {
	decision = strings.ToLower(strings.TrimSpace(decision))
	answer = strings.TrimSpace(answer)
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return fmt.Errorf("suggestion keyword is required")
	}
	switch decision {
	case classify.DecisionTier:
		simple, complex := classify.TierKeywords()
		switch answer {
		case string(classify.TierSimple):
			if len(cfg.Classifier.SimpleKeywords) == 0 {
				cfg.Classifier.SimpleKeywords = simple
			}
			cfg.Classifier.SimpleKeywords = appendUniqueKeyword(cfg.Classifier.SimpleKeywords, keyword)
		case string(classify.TierComplex):
			if len(cfg.Classifier.ComplexSignals) == 0 {
				cfg.Classifier.ComplexSignals = complex
			}
			cfg.Classifier.ComplexSignals = appendUniqueKeyword(cfg.Classifier.ComplexSignals, keyword)
		default:
			return fmt.Errorf("tier suggestion %q has no deterministic keyword target", answer)
		}
	case classify.DecisionLane:
		agentCfg, ok := cfg.Agents[answer]
		if !ok {
			return fmt.Errorf("lane suggestion target agent %q is not configured", answer)
		}
		agentCfg.LaneKeywords = appendUniqueKeyword(agentCfg.LaneKeywords, keyword)
		cfg.Agents[answer] = agentCfg
	default:
		return fmt.Errorf("classifier suggestions can update lane or tier keywords, not %q", decision)
	}
	return nil
}

func appendUniqueKeyword(list []string, keyword string) []string {
	for _, existing := range list {
		if strings.EqualFold(strings.TrimSpace(existing), keyword) {
			return list
		}
	}
	return append(list, keyword)
}

func refreshClassifierRuntimeConfig(cfg *config.Config) {
	var lanes []classify.LaneConfig
	for name, agent := range cfg.Agents {
		if len(agent.LaneKeywords) == 0 {
			continue
		}
		lanes = append(lanes, classify.LaneConfig{Name: name, Keywords: agent.LaneKeywords})
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
	classify.SetLanes(lanes)
	classify.SetTierKeywords(cfg.Classifier.SimpleKeywords, cfg.Classifier.ComplexSignals)
}
