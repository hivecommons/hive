package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// IntegratedPreflightRequest is the bounded, owner-authenticated readiness
// probe for the repository already owned by this Hive. It cannot select a
// repository or perform a lifecycle mutation.
type IntegratedPreflightRequest struct {
	Repository      string `json:"-"`
	RequestID       string `json:"request_id"`
	Provider        string `json:"provider"`
	VisualHiveRef   string `json:"visual_hive_ref"`
	Coverage        string `json:"coverage"`
	Automation      string `json:"automation"`
	MaxActiveIssues *int   `json:"max_active_issues,omitempty"`
}

func (s *Server) handleIntegratedPreflight(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil || s.deps.IntegratedPreflightFunc == nil {
		jsonError(w, "integrated preflight is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request IntegratedPreflightRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		jsonError(w, "invalid integrated preflight request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		jsonError(w, "invalid integrated preflight request: "+err.Error(), http.StatusBadRequest)
		return
	}
	operator, repository, ok := s.authorizeIntegratedOwner(w, r)
	if !ok {
		return
	}
	request.Repository = repository
	if err := validateIntegratedPreflightRequest(request); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.deps.IntegratedPreflightFunc(r.Context(), request, operator)
	if err != nil {
		s.writeIntegratedLifecycleError(w, r, "preflight", repository, err)
		return
	}
	s.auditFromRequest(r, "integrated_preflight", "", auditDetail(
		"repository", repository,
		"request_id", request.RequestID,
		"visual_hive_ref", request.VisualHiveRef,
	))
	jsonResponse(w, result)
}

func validateIntegratedPreflightRequest(request IntegratedPreflightRequest) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(request.Repository) {
		return errors.New("the hive has no exact repository configured for integrated preflight")
	}
	if !integratedRequestIDPattern.MatchString(request.RequestID) {
		return errors.New("request_id must be 8-128 characters using letters, digits, dot, underscore, colon, or hyphen")
	}
	if strings.TrimSpace(request.Provider) != "codex" {
		return errors.New("dashboard integrated preflight currently requires provider codex")
	}
	if !integratedSetupCommitPattern.MatchString(request.VisualHiveRef) {
		return errors.New("visual_hive_ref must be an exact lowercase 40-character commit")
	}
	switch request.Coverage {
	case "essential", "standard", "comprehensive", "custom":
	default:
		return errors.New("coverage must be essential, standard, comprehensive, or custom")
	}
	switch request.Automation {
	case "advisory", "issues", "repair-pr", "auto-merge":
	default:
		return errors.New("automation must be advisory, issues, repair-pr, or auto-merge")
	}
	if request.MaxActiveIssues != nil && (*request.MaxActiveIssues < 1 || *request.MaxActiveIssues > 100) {
		return errors.New("max_active_issues must be between 1 and 100 when specified")
	}
	return nil
}
