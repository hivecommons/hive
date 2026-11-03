package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
)

type classifierSettingsPayload struct {
	SimpleKeywords []string `json:"simple_keywords"`
	ComplexSignals []string `json:"complex_signals"`
}

func classifierSettingsResponse(cfg *config.Config) classifierSettingsPayload {
	if cfg == nil {
		return classifierSettingsPayload{}
	}
	return classifierSettingsPayload{
		SimpleKeywords: append([]string{}, cfg.Classifier.SimpleKeywords...),
		ComplexSignals: append([]string{}, cfg.Classifier.ComplexSignals...),
	}
}

func (s *Server) handleClassifierSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body classifierSettingsPayload
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if cfg == nil {
		jsonError(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	cfg.Classifier = config.ClassifierConfig{
		SimpleKeywords: trimNonEmpty(body.SimpleKeywords),
		ComplexSignals: trimNonEmpty(body.ComplexSignals),
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after classifier settings update", "error", err)
		jsonError(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_classifier_settings", auditDetail("section", "classifier"), "")
	jsonResponse(w, classifierSettingsResponse(cfg))
}
