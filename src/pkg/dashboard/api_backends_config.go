package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
)

type backendsPolicyPayload struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

func backendsPolicyResponse(cfg *config.Config) backendsPolicyPayload {
	if cfg == nil {
		return backendsPolicyPayload{}
	}
	return backendsPolicyPayload{
		Allow: append([]string{}, cfg.Backends.Allow...),
		Deny:  append([]string{}, cfg.Backends.Deny...),
	}
}

func (s *Server) handleBackendsPolicyPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body backendsPolicyPayload
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if cfg == nil {
		jsonError(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	next := config.BackendsConfig{
		Allow: trimNonEmpty(body.Allow),
		Deny:  trimNonEmpty(body.Deny),
	}
	for _, backend := range append(append([]string{}, next.Allow...), next.Deny...) {
		if err := cfg.Governor.ValidateBackend(backend); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	cfg.Backends = next
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after backends policy update", "error", err)
		jsonError(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "config_backends_policy", auditDetail("section", "backends"), "")
	jsonResponse(w, backendsPolicyResponse(cfg))
}
