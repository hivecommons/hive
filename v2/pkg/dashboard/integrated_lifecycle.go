package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var integratedRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// IntegratedLifecycleRequest is the closed, typed bridge between the
// authenticated dashboard and Hive's existing integrated lifecycle. The
// repository and state selection are always supplied by the server.
type IntegratedLifecycleRequest struct {
	Repository         string
	Operation          string
	Action             string
	RequestID          string
	ExpectedPlanSHA256 string
	Baseline           *IntegratedBaselineApprovalRequest
}

type IntegratedBaselineApprovalRequest struct {
	RequestID       string `json:"request_id"`
	RepositoryID    string `json:"repository_id"`
	CaptureRunID    int64  `json:"capture_run_id"`
	ArtifactID      int64  `json:"artifact_id"`
	PRNumber        int    `json:"pr_number"`
	HeadSHA         string `json:"head_sha"`
	BaseSHA         string `json:"base_sha"`
	DiffDigest      string `json:"diff_digest"`
	CandidateDigest string `json:"candidate_digest"`
	ActorID         int64  `json:"actor_id"`
	PlanDigest      string `json:"plan_digest"`
	Reason          string `json:"reason"`
}

type integratedControlRequest struct {
	Operation          string `json:"operation"`
	RequestID          string `json:"request_id"`
	ExpectedPlanSHA256 string `json:"expected_plan_sha256,omitempty"`
}

func (s *Server) handleIntegratedStatus(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedRead(w, r, "status")
}

func (s *Server) handleIntegratedDoctor(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedRead(w, r, "doctor")
}

func (s *Server) handleIntegratedRead(w http.ResponseWriter, r *http.Request, operation string) {
	if s.deps == nil || s.deps.IntegratedLifecycleFunc == nil {
		jsonError(w, "integrated lifecycle is unavailable", http.StatusServiceUnavailable)
		return
	}
	token, repository, ok := s.authorizeIntegratedOwner(w, r)
	if !ok {
		return
	}
	result, err := s.deps.IntegratedLifecycleFunc(r.Context(), IntegratedLifecycleRequest{
		Repository: repository,
		Operation:  operation,
	}, token)
	if err != nil {
		s.writeIntegratedLifecycleError(w, r, operation, repository, token, err)
		return
	}
	s.auditFromRequest(r, "integrated_"+operation, "", auditDetail("repository", repository))
	jsonResponse(w, result)
}

func (s *Server) handleIntegratedBaselinePlan(w http.ResponseWriter, r *http.Request) {
	if !decodeEmptyIntegratedRequest(w, r) {
		return
	}
	s.handleIntegratedBaseline(w, r, nil)
}

func (s *Server) handleIntegratedBaselineApprove(w http.ResponseWriter, r *http.Request) {
	var request IntegratedBaselineApprovalRequest
	if !decodeIntegratedJSON(w, r, &request) {
		return
	}
	if err := validateIntegratedBaselineApproval(request); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.handleIntegratedBaseline(w, r, &request)
}

func (s *Server) handleIntegratedBaseline(w http.ResponseWriter, r *http.Request, approval *IntegratedBaselineApprovalRequest) {
	if s.deps == nil || s.deps.IntegratedLifecycleFunc == nil {
		jsonError(w, "integrated lifecycle is unavailable", http.StatusServiceUnavailable)
		return
	}
	token, repository, ok := s.authorizeIntegratedOwner(w, r)
	if !ok {
		return
	}
	operation := "baseline-plan"
	if approval != nil {
		operation = "baseline-approve"
	}
	result, err := s.deps.IntegratedLifecycleFunc(r.Context(), IntegratedLifecycleRequest{
		Repository: repository,
		Operation:  operation,
		Baseline:   approval,
	}, token)
	if err != nil {
		s.writeIntegratedLifecycleError(w, r, operation, repository, token, err)
		return
	}
	detail := auditDetail("repository", repository)
	if approval != nil {
		detail = auditDetail(
			"repository", repository,
			"request_id", approval.RequestID,
			"repository_id", approval.RepositoryID,
			"capture_run_id", fmt.Sprint(approval.CaptureRunID),
			"artifact_id", fmt.Sprint(approval.ArtifactID),
			"pr_number", fmt.Sprint(approval.PRNumber),
			"head_sha", approval.HeadSHA,
			"plan_digest", approval.PlanDigest,
		)
	}
	s.auditFromRequest(r, "integrated_"+strings.ReplaceAll(operation, "-", "_"), "", detail)
	jsonResponse(w, result)
}

func (s *Server) handleIntegratedControlPlan(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedControl(w, r, false)
}

func (s *Server) handleIntegratedControlApply(w http.ResponseWriter, r *http.Request) {
	s.handleIntegratedControl(w, r, true)
}

func (s *Server) handleIntegratedControl(w http.ResponseWriter, r *http.Request, apply bool) {
	if s.deps == nil || s.deps.IntegratedLifecycleFunc == nil {
		jsonError(w, "integrated lifecycle is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request integratedControlRequest
	if !decodeIntegratedJSON(w, r, &request) {
		return
	}
	if err := validateIntegratedControlRequest(request, apply); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	token, repository, ok := s.authorizeIntegratedOwner(w, r)
	if !ok {
		return
	}
	operation := "control-plan"
	if apply {
		operation = "control-apply"
	}
	result, err := s.deps.IntegratedLifecycleFunc(r.Context(), IntegratedLifecycleRequest{
		Repository:         repository,
		Operation:          operation,
		Action:             request.Operation,
		RequestID:          request.RequestID,
		ExpectedPlanSHA256: request.ExpectedPlanSHA256,
		Baseline:           nil,
	}, token)
	if err != nil {
		s.writeIntegratedLifecycleError(w, r, request.Operation, repository, token, err)
		return
	}
	s.auditFromRequest(r, "integrated_control_"+map[bool]string{false: "plan", true: "apply"}[apply], "", auditDetail(
		"repository", repository,
		"operation", request.Operation,
		"request_id", request.RequestID,
		"expected_plan_sha256", request.ExpectedPlanSHA256,
	))
	jsonResponse(w, result)
}

func validateIntegratedControlRequest(request integratedControlRequest, apply bool) error {
	switch request.Operation {
	case "trigger", "pause", "resume", "uninstall", "uninstall-finalize", "uninstall-cancel":
	default:
		return errors.New("operation must be trigger, pause, resume, uninstall, uninstall-finalize, or uninstall-cancel")
	}
	if !integratedRequestIDPattern.MatchString(request.RequestID) {
		return errors.New("request_id must be 8-128 characters using letters, digits, dot, underscore, colon, or hyphen")
	}
	if apply {
		if !integratedSetupDigestPattern.MatchString(request.ExpectedPlanSHA256) {
			return errors.New("expected_plan_sha256 must bind the exact dashboard control plan")
		}
	} else if request.ExpectedPlanSHA256 != "" {
		return errors.New("expected_plan_sha256 is accepted only by control apply")
	}
	return nil
}

func validateIntegratedBaselineApproval(request IntegratedBaselineApprovalRequest) error {
	if !integratedRequestIDPattern.MatchString(request.RequestID) {
		return errors.New("request_id must be 8-128 characters using letters, digits, dot, underscore, colon, or hyphen")
	}
	if !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(request.RepositoryID) || request.CaptureRunID <= 0 || request.ArtifactID <= 0 ||
		request.PRNumber <= 0 || request.ActorID <= 0 {
		return errors.New("baseline approval requires positive repository, run, artifact, PR, and actor identities")
	}
	for name, value := range map[string]string{
		"head_sha": request.HeadSHA,
		"base_sha": request.BaseSHA,
	} {
		if !integratedSetupCommitPattern.MatchString(value) {
			return fmt.Errorf("%s must be an exact lowercase 40-character commit", name)
		}
	}
	for name, value := range map[string]string{
		"diff_digest":      request.DiffDigest,
		"candidate_digest": request.CandidateDigest,
		"plan_digest":      request.PlanDigest,
	} {
		if !integratedSetupDigestPattern.MatchString(value) {
			return fmt.Errorf("%s must be an exact lowercase SHA-256 digest", name)
		}
	}
	reason := strings.TrimSpace(request.Reason)
	if len(reason) < 12 || len(reason) > 500 || strings.ContainsAny(reason, "\r\n") {
		return errors.New("reason must be a single accountable line from 12 through 500 characters")
	}
	return nil
}

func decodeEmptyIntegratedRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	var request struct{}
	return decodeIntegratedJSON(w, r, &request)
}

func decodeIntegratedJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		jsonError(w, "invalid integrated lifecycle request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	if err := ensureJSONEOF(decoder); err != nil {
		jsonError(w, "invalid integrated lifecycle request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) writeIntegratedLifecycleError(w http.ResponseWriter, r *http.Request, operation, repository, token string, err error) {
	reference := digestSetupError(err)
	s.auditFromRequest(r, "integrated_"+strings.ReplaceAll(operation, "-", "_")+"_failed", "", auditDetail(
		"repository", repository,
		"error_sha256", reference,
	))
	jsonError(w, "integrated lifecycle operation failed: "+scrubIntegratedLifecycleError(err, token)+" (reference "+reference[:12]+")", http.StatusConflict)
}

func scrubIntegratedLifecycleError(err error, token string) string {
	message := strings.TrimSpace(fmt.Sprint(err))
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	message = regexp.MustCompile(`(?i)(bearer[ =:]+|token[ =:]+|sk-(?:proj-)?)[A-Za-z0-9._-]+`).ReplaceAllString(message, "$1[redacted]")
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return "operation failed without a diagnostic"
	}
	if len(message) > 500 {
		return message[:500] + "..."
	}
	return message
}
