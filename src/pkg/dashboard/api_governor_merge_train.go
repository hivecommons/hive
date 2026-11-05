package dashboard

import (
	"net/http"
	"time"
)

// handleGovernorMergeTrainPatch edits the stall warning without changing other governor settings.
func (s *Server) handleGovernorMergeTrainPatch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		StallWarnS *int `json:"merge_train_stall_warn_s"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.StallWarnS != nil && (*body.StallWarnS < 0 || int64(*body.StallWarnS) > int64((1<<63-1)/time.Second)) {
		jsonError(w, "merge_train_stall_warn_s must be non-negative seconds within the supported duration range", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	previous := cfg.Governor
	if body.StallWarnS != nil {
		cfg.SetMergeTrainStallWarnS(*body.StallWarnS)
	}
	if err := s.saveConfig(); err != nil {
		cfg.Governor = previous
		s.logger.Error("failed to persist merge-train stall warning", "error", err)
		jsonError(w, "failed to persist governor settings", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_governor", auditDetail("section", "governor"), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{"merge_train_stall_warn_s": int(cfg.MergeTrainStallWarn() / time.Second)})
}
